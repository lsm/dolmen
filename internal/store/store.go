package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"

	_ "modernc.org/sqlite"
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
	dir string
	mu  ctxMutex
	nss map[string]*nsDB

	notifyMu  sync.Mutex
	listeners map[string][]*commitListener

	listenSessions map[string][]*listenSession
	pruneNext      map[string]time.Time

	changeRetention time.Duration

	closed   atomic.Bool
	closeErr error
}

type nsDB struct {
	rw *sql.DB
	ro *sql.DB
}

type OpenOption func(*Store)

func WithChangeRetention(d time.Duration) OpenOption {
	return func(s *Store) { s.changeRetention = d }
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
	s := &Store{dir: abs, mu: newCtxMutex(), nss: map[string]*nsDB{}, changeRetention: DefaultChangeRetention}
	for _, opt := range opts {
		opt(s)
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
		return nil
	}
	defer ro.Close()
	format, minReader, err := readCatalogVersion(ctx, ro)
	if err != nil {
		return nil
	}
	if minReader > CatalogFormat {
		return &CatalogVersionError{Namespace: name, Format: format, MinReader: minReader, Supported: CatalogFormat}
	}
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
	return s.lockedNS(name)
}

func (s *Store) nsCtx(ctx context.Context, name string) (*nsDB, error) {
	if err := validateNSPath(name); err != nil {
		return nil, err
	}
	if err := s.mu.LockCtx(ctx); err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	return s.lockedNSCtx(ctx, name)
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
	rw, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, err
	}
	rw.SetMaxOpenConns(1)
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
	`CREATE TABLE IF NOT EXISTS _dolmen_idempotency(
		table_name TEXT NOT NULL,
		key TEXT NOT NULL,
		payload_hash TEXT NOT NULL,
		ids_json TEXT NOT NULL,
		at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
		PRIMARY KEY(table_name, key)
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

	`CREATE INDEX IF NOT EXISTS _dolmen_changes_at ON _dolmen_changes(at)`,

	`CREATE INDEX IF NOT EXISTS _dolmen_cursor_tokens_issued_at ON _dolmen_cursor_tokens(issued_at)`,
	`CREATE INDEX IF NOT EXISTS _dolmen_cursor_tokens_chain_start ON _dolmen_cursor_tokens(chain_start)`,
	`CREATE INDEX IF NOT EXISTS _dolmen_cursor_tokens_chain_origin ON _dolmen_cursor_tokens(chain_origin)`,
}

func dsn(path string, readonly bool) string {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	if readonly {
		q.Add("mode", "ro")
	} else {

		q.Add("mode", "rw")
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(NORMAL)")

		q.Add("_txlock", "immediate")
	}
	u.RawQuery = q.Encode()
	return u.String()
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

func loadSchema(ctx context.Context, db rowQuerier, nsName, table string) (*schema.TableSchema, error) {

	var raw string
	err := db.QueryRowContext(ctx,
		`SELECT schema_json FROM _dolmen_tables WHERE name = ?`, table).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: table %s.%s", ErrNotFound, nsName, table)
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
	raw, err := json.Marshal(sc)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE _dolmen_tables SET version = ?, schema_json = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE name = ?`,
		sc.Version, string(raw), sc.Name); err != nil {
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
	sc, err := loadSchema(ctx, n.ro, nsName, table)
	if err != nil {
		return nil, 0, err
	}
	if err := checkScopeIncarnation(ctx, n.ro, nsName, table, scopeIncarnation); err != nil {
		return nil, 0, err
	}
	if err := ScopeUsable(scope, sc); err != nil {
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
