package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var ErrNotFound = errors.New("not found")

var ErrInvalid = errors.New("invalid request")

var ErrClosed = errors.New("store is closed")

var ErrExists = errors.New("already exists")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

func conflictf(format string, args ...any) error {
	return derr.Wrap(derr.Conflict, invalidf(format, args...))
}

const nsSegmentSrc = `[a-z0-9][a-z0-9_-]{0,63}`

var nsSegmentRe = regexp.MustCompile(`^` + nsSegmentSrc + `$`)

const maxNSDepth = 3

func NSPathPattern() string {
	return fmt.Sprintf(`^%s(/%s){0,%d}$`, nsSegmentSrc, nsSegmentSrc, maxNSDepth-1)
}

const DefaultChangeRetention = 168 * time.Hour

const (
	DefaultMaxOpenNamespaces = 128
	readConnsPerNS           = 16
	idleReadConnsPerNS       = 2
)

type ctxMutex struct {
	ch chan struct{}
}

func newCtxMutex() ctxMutex {
	ch := make(chan struct{}, 1)
	ch <- struct{}{}
	return ctxMutex{ch: ch}
}

func (m *ctxMutex) Lock() {
	<-m.ch
}

func (m *ctxMutex) LockCtx(ctx context.Context) error {
	select {
	case <-m.ch:
		return nil
	default:
	}
	select {
	case <-m.ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *ctxMutex) Unlock() {
	m.ch <- struct{}{}
}

type Store struct {
	dir      string
	mu       ctxMutex
	nss      map[string]*nsDB
	maxOpen  int
	useTick  uint64
	sync     SyncMode
	maxBytes int64

	unreadableMu sync.Mutex
	unreadable   []string

	notifyMu  sync.Mutex
	listeners map[string][]*commitListener

	listenSessions map[string][]*listenSession
	pruneNext      map[string]time.Time

	changeRetention time.Duration

	tok    tokenizer
	vcache vecCache

	closed   atomic.Bool
	closeErr error
}

type nsDB struct {
	rw      *sql.DB
	ro      *sql.DB
	pins    atomic.Int64
	lastUse uint64
}

func (n *nsDB) unpin() {
	n.pins.Add(-1)
}

type OpenOption func(*Store)

func WithChangeRetention(d time.Duration) OpenOption {
	return func(s *Store) { s.changeRetention = d }
}

func WithMaxOpenNamespaces(n int) OpenOption {
	return func(s *Store) { s.maxOpen = n }
}

var detectNetworkFS = networkFilesystem

func probeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".dolmen-probe-*")
	if err != nil {
		return fmt.Errorf("data directory %s is not writable (check its owner, permissions, and that the volume is not mounted read-only): %w", dir, err)
	}
	name := f.Name()
	f.Close()
	return os.Remove(name)
}

func Open(dir string, opts ...OpenOption) (*Store, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(abs, 0o700); err != nil {
		return nil, fmt.Errorf("cannot secure data directory %s (owner-only permissions): %w", abs, err)
	}
	if err := probeWritable(abs); err != nil {
		return nil, err
	}
	if fs, remote := detectNetworkFS(abs); remote {
		slog.Warn("data directory is on a network filesystem; SQLite WAL needs local shared memory and file locks, so concurrent access can corrupt data. Move the data directory to a local disk", "dir", abs, "filesystem", fs)
	}
	s := &Store{dir: abs, mu: newCtxMutex(), nss: map[string]*nsDB{}, maxOpen: DefaultMaxOpenNamespaces, vcache: vecCache{max: DefaultVectorCacheBytes}, sync: DefaultSync, changeRetention: DefaultChangeRetention}
	for _, opt := range opts {
		opt(s)
	}
	if s.maxBytes < 0 {
		return nil, fmt.Errorf("max namespace size must not be negative (0 means unbounded), got %d", s.maxBytes)
	}
	mode, err := ParseSyncMode(string(s.sync))
	if err != nil {
		return nil, err
	}
	s.sync = mode
	if s.maxOpen < 1 {
		return nil, fmt.Errorf("max open namespaces must be at least 1, got %d", s.maxOpen)
	}
	if err := s.verifyCatalogVersions(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) verifyCatalogVersions(ctx context.Context) error {
	var names []string
	if err := walkNamespaces(s.dir, "", &names); err != nil {
		return err
	}
	sortNS(names)
	for _, name := range names {
		if err := s.verifyOneCatalogVersion(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) verifyOneCatalogVersion(ctx context.Context, name string) error {
	ro, err := sql.Open("sqlite", dsn(s.nsPath(name), true))
	if err != nil {
		slog.Warn("namespace is unreadable; requests to it fail until it is repaired", "namespace", name, "err", err)
		s.unreadable = append(s.unreadable, name)
		return nil
	}
	defer ro.Close()
	format, minReader, err := readCatalogVersion(ctx, ro)
	if err != nil {
		slog.Warn("namespace is unreadable; requests to it fail until it is repaired", "namespace", name, "err", err)
		s.unreadable = append(s.unreadable, name)
		return nil
	}
	if minReader > CatalogFormat {
		return &CatalogVersionError{Namespace: name, Format: format, MinReader: minReader, Supported: CatalogFormat}
	}
	warnCatalogDrift(ctx, ro, name)
	return nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return s.closeErr
	}
	s.closed.Store(true)
	s.closeErr = s.lockedClose()
	if s.tok.db != nil {
		s.tok.db.Close()
	}
	return s.closeErr
}

func (s *Store) lockedClose() error {
	var first error
	names := make([]string, 0, len(s.nss))
	for name, n := range s.nss {
		if err := n.rw.Close(); err != nil && first == nil {
			first = err
		}
		if err := n.ro.Close(); err != nil && first == nil {
			first = err
		}
		delete(s.nss, name)
		names = append(names, name)
	}
	for _, name := range names {
		s.endListenSessions(name, ErrListenLifetimeEnded)
	}
	return first
}

func (s *Store) nsEvicted(name string, want *nsDB) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nss[name] != want
}

func (s *Store) ns(name string) (*nsDB, error) {
	if err := validateNSPath(name); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pinLocked(s.lockedNS(name))
}

func (s *Store) nsCtx(ctx context.Context, name string) (*nsDB, error) {
	if err := validateNSPath(name); err != nil {
		return nil, err
	}
	if err := s.mu.LockCtx(ctx); err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	return s.pinLocked(s.lockedNSCtx(ctx, name))
}

func (s *Store) pinLocked(n *nsDB, err error) (*nsDB, error) {
	if err != nil {
		return nil, err
	}
	n.pins.Add(1)
	s.useTick++
	n.lastUse = s.useTick
	return n, nil
}

func (s *Store) evictIdleLocked() {
	for len(s.nss) >= s.maxOpen {
		var victim string
		var oldest *nsDB
		for name, n := range s.nss {
			if n.pins.Load() > 0 || n.ro.Stats().InUse > 0 || n.rw.Stats().InUse > 0 {
				continue
			}
			if oldest == nil || n.lastUse < oldest.lastUse {
				victim, oldest = name, n
			}
		}
		if oldest == nil {
			return
		}
		if err := oldest.ro.Close(); err != nil {
			slog.Warn("closing an idle namespace's read pool failed", "namespace", victim, "err", err)
		}
		if err := oldest.rw.Close(); err != nil {
			slog.Warn("closing an idle namespace's write connection failed", "namespace", victim, "err", err)
		}
		delete(s.nss, victim)
	}
}

func (s *Store) lockedNS(name string) (*nsDB, error) {
	return s.lockedNSCtx(context.Background(), name)
}

func (s *Store) lockedNSCtx(ctx context.Context, name string) (*nsDB, error) {
	if s.closed.Load() {
		return nil, ErrClosed
	}
	if n, ok := s.nss[name]; ok {
		return n, nil
	}
	if err := s.verifyNSDirs(name); err != nil {
		return nil, err
	}
	path := s.nsPath(name)

	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: namespace %s does not exist; create it with create_namespace, or let any write op (create_table, insert, update, upsert, upsert_by_key, delete, migrate) create it on first use", ErrNotFound, name)
		}
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, invalidf("namespace %s: %s is not a regular file", name, path)
	}
	s.evictIdleLocked()
	rw, err := s.openWriter(ctx, path)
	if err != nil {
		return nil, err
	}
	rw.SetMaxOpenConns(1)
	rw.SetMaxIdleConns(1)
	if err := verifySync(ctx, rw, name, s.sync); err != nil {
		rw.Close()
		return nil, err
	}
	if err := refuseNewerCatalog(ctx, rw, name); err != nil {
		rw.Close()
		return nil, err
	}
	for _, ddl := range registryDDL {
		if _, err := rw.ExecContext(ctx, ddl); err != nil {
			rw.Close()
			return nil, fmt.Errorf("init namespace %s: %w", name, err)
		}
	}

	if err := ensureCatalogVersion(ctx, rw, name); err != nil {
		rw.Close()
		var cve *CatalogVersionError
		if errors.As(err, &cve) {
			return nil, err
		}
		return nil, fmt.Errorf("init namespace %s: %w", name, err)
	}

	if err := ensureIdempotencyOwner(ctx, rw); err != nil {
		rw.Close()
		return nil, fmt.Errorf("init namespace %s: %w", name, err)
	}

	if err := ensureNSGen(ctx, rw); err != nil {
		rw.Close()
		return nil, fmt.Errorf("init namespace %s: %w", name, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		rw.Close()
		return nil, fmt.Errorf("cannot secure namespace db %s (owner-only permissions): %w", path, err)
	}
	for _, side := range []string{path + "-wal", path + "-shm"} {
		if _, statErr := os.Stat(side); statErr == nil {
			if err := os.Chmod(side, 0o600); err != nil {
				rw.Close()
				return nil, fmt.Errorf("cannot secure %s (owner-only permissions): %w", side, err)
			}
		}
	}
	ro, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		rw.Close()
		return nil, err
	}
	ro.SetMaxOpenConns(readConnsPerNS)
	ro.SetMaxIdleConns(idleReadConnsPerNS)
	n := &nsDB{rw: rw, ro: ro}
	s.nss[name] = n
	return n, nil
}

var registryDDL = []string{
	`CREATE TABLE IF NOT EXISTS _dolmen_tables(
		name TEXT PRIMARY KEY,
		version INTEGER NOT NULL,
		schema_json TEXT NOT NULL,
		updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
	)`,
	`CREATE TABLE IF NOT EXISTS _dolmen_migrations(
		id INTEGER PRIMARY KEY,
		table_name TEXT NOT NULL,
		from_version INTEGER NOT NULL,
		to_version INTEGER NOT NULL,
		changes_json TEXT NOT NULL,
		at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
	)`,
	`CREATE TABLE IF NOT EXISTS _dolmen_idempotency_owned(
		table_name TEXT NOT NULL,
		owner TEXT NOT NULL DEFAULT '',
		key TEXT NOT NULL,
		payload_hash TEXT NOT NULL,
		ids_json TEXT NOT NULL,
		at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
		PRIMARY KEY(table_name, owner, key)
	)`,

	`CREATE TABLE IF NOT EXISTS _dolmen_drop_gen(
		table_name TEXT PRIMARY KEY,
		gen INTEGER NOT NULL
	)`,

	`CREATE TABLE IF NOT EXISTS _dolmen_meta(
		key TEXT PRIMARY KEY,
		value BLOB
	)`,

	`CREATE TABLE IF NOT EXISTS _dolmen_changes(
		seq INTEGER PRIMARY KEY AUTOINCREMENT,
		table_name TEXT NOT NULL,
		row_id INTEGER NOT NULL,
		kind TEXT NOT NULL,
		owner TEXT,
		nsgen BLOB NOT NULL,
		drop_gen INTEGER NOT NULL,
		at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
	)`,

	`CREATE TABLE IF NOT EXISTS _dolmen_cursor_tokens(
		token TEXT PRIMARY KEY,
		position INTEGER NOT NULL,
		issued_at INTEGER NOT NULL,
		chain_id TEXT NOT NULL,
		chain_origin INTEGER NOT NULL,
		chain_start INTEGER NOT NULL,
		feed_table TEXT NOT NULL DEFAULT ''
	)`,

	`CREATE INDEX IF NOT EXISTS _dolmen_changes_table_feed
		ON _dolmen_changes(table_name, drop_gen, nsgen, seq)`,

	`CREATE INDEX IF NOT EXISTS _dolmen_changes_owner_feed
		ON _dolmen_changes(table_name, drop_gen, nsgen, owner, seq)`,

	`CREATE INDEX IF NOT EXISTS _dolmen_changes_at ON _dolmen_changes(at)`,

	`CREATE INDEX IF NOT EXISTS _dolmen_cursor_tokens_issued_at ON _dolmen_cursor_tokens(issued_at)`,
	`CREATE INDEX IF NOT EXISTS _dolmen_cursor_tokens_chain_start ON _dolmen_cursor_tokens(chain_start)`,
	`CREATE INDEX IF NOT EXISTS _dolmen_cursor_tokens_chain_origin ON _dolmen_cursor_tokens(chain_origin)`,
}

type SyncMode string

const (
	SyncFull    SyncMode = "full"
	SyncNormal  SyncMode = "normal"
	DefaultSync          = SyncFull
)

func ParseSyncMode(raw string) (SyncMode, error) {
	switch m := SyncMode(strings.ToLower(strings.TrimSpace(raw))); m {
	case SyncFull, SyncNormal:
		return m, nil
	}
	return "", fmt.Errorf("invalid sync mode %q: use full (a commit survives power loss once acknowledged) or normal (a commit survives a process crash; the last commits before a power loss may be lost)", raw)
}

func (m SyncMode) pragma() int {
	if m == SyncNormal {
		return 1
	}
	return 2
}

func WithMaxNamespaceSize(bytes int64) OpenOption {
	return func(s *Store) { s.maxBytes = bytes }
}

func WithVectorCacheBytes(n int64) OpenOption {
	return func(s *Store) { s.vcache.max = n }
}

func WithSync(m SyncMode) OpenOption {
	return func(s *Store) { s.sync = m }
}

func dsn(path string, readonly bool) string {
	if !readonly {
		return writerDSN(path, DefaultSync)
	}
	u := url.URL{Scheme: "file", Path: sqliteURIPath(path)}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	if readonly {
		q.Add("mode", "ro")
	} else {

		q.Add("mode", "rw")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

const walSizeLimit = 64 << 20

func writerDSN(path string, m SyncMode) string {
	return writerDSNPages(path, m, 0)
}

func writerDSNPages(path string, m SyncMode, maxPages int64) string {
	u := url.URL{Scheme: "file", Path: sqliteURIPath(path)}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("mode", "rw")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", fmt.Sprintf("synchronous(%d)", m.pragma()))
	q.Add("_pragma", fmt.Sprintf("journal_size_limit(%d)", walSizeLimit))
	if maxPages > 0 {
		q.Add("_pragma", fmt.Sprintf("max_page_count(%d)", maxPages))
	}
	q.Add("_txlock", "immediate")
	u.RawQuery = q.Encode()
	return u.String()
}

func verifySync(ctx context.Context, db *sql.DB, name string, m SyncMode) error {
	var got int
	if err := db.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&got); err != nil {
		return err
	}
	if got != m.pragma() {
		return fmt.Errorf("namespace %s runs with synchronous=%d, not the %d that sync mode %s requires", name, got, m.pragma(), m)
	}
	return nil
}

func q(name string) string {
	return `"` + name + `"`
}

func ftsTable(table string) string {
	return table + "__fts"
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func TableNotFound(nsName, table string) error {
	if err := schema.ValidateTableName(table); err != nil {
		return fmt.Errorf("%w: %v; list_tables shows the tables %s holds", ErrInvalid, err, nsName)
	}
	return fmt.Errorf("%w: table %s.%s does not exist; list_tables shows the tables %s holds", ErrNotFound, nsName, table, nsName)
}

func loadSchema(ctx context.Context, db rowQuerier, nsName, table string) (*schema.TableSchema, error) {

	var raw string
	err := db.QueryRowContext(ctx,
		`SELECT schema_json FROM _dolmen_tables WHERE name = ?`, table).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, TableNotFound(nsName, table)
	}
	if err != nil {
		return nil, err
	}
	var sc schema.TableSchema

	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&sc); err != nil {
		return nil, fmt.Errorf("corrupt schema for %s.%s: %w", nsName, table, err)
	}
	return &sc, nil
}

func saveSchemaTx(ctx context.Context, tx *sql.Tx, nsName string, sc *schema.TableSchema, fromVersion int, changes any) error {
	raw, err := encodeSchemaOver(ctx, tx, sc.Name, sc, RenamesOf(changes))
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE _dolmen_tables SET version = ?, schema_json = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE name = ?`,
		sc.Version, raw, sc.Name); err != nil {
		return err
	}
	cj, err := json.Marshal(changes)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO _dolmen_migrations(table_name, from_version, to_version, changes_json) VALUES(?,?,?,?)`,
		sc.Name, fromVersion, sc.Version, string(cj))
	return err
}

func (s *Store) ListTables(ctx context.Context, nsName string, bindings []AuthBinding) ([]string, error) {
	n, err := s.ns(nsName)
	if err != nil {
		return nil, err
	}
	defer n.unpin()
	rows, err := n.ro.QueryContext(ctx, `SELECT name FROM _dolmen_tables ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

type rowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func registeredTables(ctx context.Context, db rowsQuerier) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM _dolmen_tables`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

func (s *Store) DescribeTable(ctx context.Context, nsName, table string, scope *RowScope, scopeIncarnation Incarnation) (*schema.TableSchema, int64, error) {
	n, err := s.ns(nsName)
	if err != nil {
		return nil, 0, err
	}
	defer n.unpin()
	sc, err := loadSchema(ctx, n.ro, nsName, table)
	if err != nil {
		return nil, 0, err
	}
	if err := checkScopeIncarnation(ctx, n.ro, nsName, table, scopeIncarnation); err != nil {
		return nil, 0, err
	}
	if err := scopeUsable(scope, sc); err != nil {
		return nil, 0, err
	}
	if scope != nil && scope.Empty {
		return sc, 0, nil
	}
	countStmt := fmt.Sprintf(`SELECT count(*) FROM %s`, q(table))
	var cargs []any
	if clause, sargs := scopeClause(scope, ""); clause != "" {
		countStmt = fmt.Sprintf(`SELECT count(*) FROM %s WHERE %s`, q(table), clause)
		cargs = sargs
	}
	var count int64
	if err := n.ro.QueryRowContext(ctx, countStmt, cargs...).Scan(&count); err != nil {
		return nil, 0, err
	}
	return sc, count, nil
}

type Migration struct {
	ID          int64           `json:"id"`
	FromVersion int             `json:"from_version"`
	ToVersion   int             `json:"to_version"`
	Changes     []schema.Change `json:"changes"`
	At          string          `json:"at"`
}

func (s *Store) ListMigrations(ctx context.Context, nsName, table string, inc Incarnation) ([]Migration, error) {
	n, err := s.ns(nsName)
	if err != nil {
		return nil, err
	}
	defer n.unpin()
	if _, err := loadSchema(ctx, n.ro, nsName, table); err != nil {
		return nil, err
	}
	rows, err := n.ro.QueryContext(ctx,
		`SELECT id, from_version, to_version, changes_json, at FROM _dolmen_migrations WHERE table_name = ? ORDER BY id DESC`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Migration{}
	for rows.Next() {
		var m Migration
		var cj string
		if err := rows.Scan(&m.ID, &m.FromVersion, &m.ToVersion, &cj, &m.At); err != nil {
			return nil, err
		}

		dec := json.NewDecoder(strings.NewReader(cj))
		dec.UseNumber()
		if err := dec.Decode(&m.Changes); err != nil {
			return nil, fmt.Errorf("corrupt migration record %d for %s.%s: %w", m.ID, nsName, table, err)
		}

		for j := range m.Changes {
			if !schema.TakesValue(m.Changes[j].Op) {
				m.Changes[j].Value = nil
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

const MaxFieldsPerTable = 100

func (s *Store) CreateTable(ctx context.Context, nsName, table string, fields []schema.Field, opts TableOpts, nsGen [16]byte) (*schema.TableSchema, error) {
	var err error
	fields, err = ValidateTableDefinition(table, fields)
	if err != nil {
		return nil, err
	}
	if err := schema.ValidateRowAccess(opts.RowAccess); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if opts.RowAccess != "" {
		if err := ValidateOwnerCollision(fields); err != nil {
			return nil, err
		}
	}
	n, err := s.ns(nsName)
	if err != nil {
		return nil, err
	}
	defer n.unpin()

	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := loadSchema(ctx, tx, nsName, table); err == nil {
		return nil, invalidf("table %s.%s already exists", nsName, table)
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, tableDDL(table, fields, opts.RowAccess != "")); err != nil {
		return nil, err
	}
	if fts := ftsFields(fields); len(fts) > 0 {
		if err := createFTS(ctx, tx, table, fts); err != nil {
			return nil, err
		}
	}
	sc := &schema.TableSchema{Namespace: nsName, Name: table, Version: 1, Fields: fields,
		RowAccess: opts.RowAccess, HasOwner: opts.RowAccess != ""}
	raw, _ := json.Marshal(sc)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO _dolmen_tables(name, version, schema_json) VALUES(?,?,?)`,
		table, 1, string(raw)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return sc, nil
}

func validateFieldDefaults(fields []schema.Field) error {
	for _, f := range fields {
		if f.Default == nil {
			continue
		}
		if schema.IsNowDefault(f.Default) {
			continue
		}
		cv, err := coerceValue(f, f.Default)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}

		if fv, isFloat := cv.(float64); isFloat && (math.IsNaN(fv) || math.IsInf(fv, 0)) {
			return invalidf("field %q: default must be a finite number", f.Name)
		}

		if sv, isStr := cv.(string); isStr && strings.ContainsRune(sv, 0) {
			return invalidf("field %q: default must not contain NUL bytes", f.Name)
		}
	}
	return nil
}

func tableDDL(table string, fields []schema.Field, owner bool) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf(`CREATE TABLE %s (id INTEGER PRIMARY KEY AUTOINCREMENT, created_at TEXT NOT NULL DEFAULT (strftime('%%Y-%%m-%%dT%%H:%%M:%%fZ','now'))`, q(table)))
	if owner {
		sb.WriteString(fmt.Sprintf(", %s TEXT", q(schema.OwnerColumn)))
	}
	for _, f := range fields {
		sb.WriteString(fmt.Sprintf(`, %s %s`, q(f.Name), schema.SQLType(f)))
		if f.Required {
			sb.WriteString(` NOT NULL`)
		}
	}
	if vecField := vectorizeField(fields); vecField != nil {
		sb.WriteString(`, "_embedding" BLOB`)
	}
	sb.WriteString(`)`)
	return sb.String()
}

func ftsFields(fields []schema.Field) []schema.Field {
	var out []schema.Field
	for _, f := range fields {
		if f.Fulltext {
			out = append(out, f)
		}
	}
	return out
}

func vectorizeField(fields []schema.Field) *schema.Field {
	for i := range fields {
		if fields[i].Vectorize {
			return &fields[i]
		}
	}
	return nil
}

func createFTS(ctx context.Context, tx *sql.Tx, table string, fts []schema.Field) error {
	cols := make([]string, len(fts))
	for i, f := range fts {
		cols[i] = q(f.Name)
	}
	ddl := fmt.Sprintf(`CREATE VIRTUAL TABLE %s USING fts5(%s, tokenize='porter unicode61')`, q(ftsTable(table)), strings.Join(cols, ", "))
	if _, err := tx.ExecContext(ctx, ddl); err != nil {
		return err
	}
	return repopulateFTS(ctx, tx, table, fts)
}

func repopulateFTS(ctx context.Context, tx *sql.Tx, table string, fts []schema.Field) error {
	cols := make([]string, len(fts))
	sel := make([]string, len(fts))
	where := make([]string, len(fts))
	for i, f := range fts {
		cols[i] = q(f.Name)
		sel[i] = q(f.Name)
		where[i] = fmt.Sprintf(`%s IS NOT NULL`, q(f.Name))
	}
	stmt := fmt.Sprintf(`INSERT INTO %s(rowid, %s) SELECT id, %s FROM %s WHERE %s`,
		q(ftsTable(table)), strings.Join(cols, ", "), strings.Join(sel, ", "), q(table), strings.Join(where, " OR "))
	_, err := tx.ExecContext(ctx, stmt)
	return err
}

func dropFTS(ctx context.Context, tx *sql.Tx, table string) error {
	_, err := tx.ExecContext(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, q(ftsTable(table))))
	return err
}

func (s *Store) Ready(ctx context.Context) error {
	if s.closed.Load() {
		return ErrClosed
	}
	f, err := os.CreateTemp(s.dir, ".ready-*")
	if err != nil {
		return fmt.Errorf("the data directory is not writable, so no write can succeed: %w", err)
	}
	name := f.Name()
	f.Close()
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("the data directory is not writable, so no write can succeed: %w", err)
	}
	if n := s.stillUnreadable(ctx); n > 0 {
		return fmt.Errorf("%d namespace(s) cannot be read (the startup log names them); repair or remove the file and readiness recovers on its own", n)
	}
	return ctx.Err()
}

func (s *Store) stillUnreadable(ctx context.Context) int {
	s.unreadableMu.Lock()
	defer s.unreadableMu.Unlock()
	kept := s.unreadable[:0]
	for _, name := range s.unreadable {
		if s.namespaceUnreadable(ctx, name) {
			kept = append(kept, name)
		}
	}
	s.unreadable = kept
	return len(kept)
}

func (s *Store) namespaceUnreadable(ctx context.Context, name string) bool {
	if _, err := os.Stat(s.nsPath(name)); errors.Is(err, os.ErrNotExist) {
		return false
	}
	ro, err := sql.Open("sqlite", dsn(s.nsPath(name), true))
	if err != nil {
		return true
	}
	defer ro.Close()
	_, minReader, err := readCatalogVersion(ctx, ro)
	return err != nil || minReader > CatalogFormat
}

func sqliteURIPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

func (s *Store) openWriter(ctx context.Context, path string) (*sql.DB, error) {
	rw, err := sql.Open("sqlite", writerDSN(path, s.sync))
	if err != nil || s.maxBytes <= 0 {
		return rw, err
	}
	var pageSize int64
	err = rw.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize)
	rw.Close()
	if err != nil {
		return nil, err
	}
	pages := s.maxBytes / pageSize
	if pages < 1 {
		pages = 1
	}
	return sql.Open("sqlite", writerDSNPages(path, s.sync, pages))
}

func ParseSize(raw string) (int64, error) {
	t := strings.TrimSpace(raw)
	units := []struct {
		suffix string
		mult   int64
	}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"B", 1}}
	mult := int64(1)
	for _, u := range units {
		if strings.HasSuffix(t, u.suffix) {
			t, mult = strings.TrimSpace(strings.TrimSuffix(t, u.suffix)), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil || n < 0 || n > (1<<62)/mult {
		return 0, fmt.Errorf("invalid size %q: use a whole number of bytes, optionally with KiB, MiB, GiB or TiB (0 means unbounded)", raw)
	}
	return n * mult, nil
}

func IsFull(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_FULL
}
