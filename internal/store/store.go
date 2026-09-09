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
	"time"

	"github.com/lsm/dolmen/internal/schema"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("not found")

var ErrInvalid = errors.New("invalid request")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

// nsSegmentSrc is ONE namespace-path segment's grammar (§5.1) — v0.2.0's
// single-segment grammar, unchanged — unanchored so NSPathPattern can
// compose segments into the path form.
const nsSegmentSrc = `[a-z0-9][a-z0-9_-]{0,63}`

// nsSegmentRe matches ONE segment of a namespace path (§5.1). validateNSPath
// composes 1–3 of these into a path; the listing walk matches it against file
// stems and against the directory names it descends through.
var nsSegmentRe = regexp.MustCompile(`^` + nsSegmentSrc + `$`)

// maxNSDepth is §5.1's namespace depth cap: a path is 1–3 segments
// (a/b/c). validateNSPath enforces it at every entry point; the listing
// walk uses it to bound descent.
const maxNSDepth = 3

// NSPathPattern is §5.1's namespace-path grammar as a JSON Schema pattern:
// 1–maxNSDepth nsSegmentSrc segments joined by single slashes, so an empty
// segment — leading, trailing, or doubled slashes — fails to match. The
// API's namespace surfaces (nsProp, list_namespaces' prefix, the OpenAPI
// TableSchema) declare it so schema-validating clients accept exactly the
// paths validateNSPath admits; a single segment still matches, which keeps
// the widened surfaces additive to v0.2.0's contract (§8.1).
func NSPathPattern() string {
	return fmt.Sprintf(`^%s(/%s){0,%d}$`, nsSegmentSrc, nsSegmentSrc, maxNSDepth-1)
}

// DefaultChangeRetention is the change log's retention bound R (§9.3) when no
// option overrides it — the documented -change-retention default, so a store
// wired without options behaves exactly like a default deployment.
const DefaultChangeRetention = 168 * time.Hour

// ctxMutex is the store's registry lock: a one-slot channel semaphore with
// a context-aware acquire. Plain Lock serves the paths that have no context
// to honor (lifecycle, Close); LockCtx lets bounded callers — wait_for's
// feed reads — abort while the lock is held, which a sync.Mutex wait never
// can (a draining drop_namespace evicts pools under this lock and can hold
// it for as long as its in-flight connections run).
type ctxMutex struct {
	ch chan struct{}
}

func newCtxMutex() ctxMutex {
	ch := make(chan struct{}, 1)
	ch <- struct{}{}
	return ctxMutex{ch: ch}
}

// Lock acquires without a context, like a sync.Mutex.
func (m *ctxMutex) Lock() {
	<-m.ch
}

// LockCtx acquires the lock or fails with the context's error — a bounded
// caller never queues behind a long holder past its own deadline.
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

	// notifyMu guards listeners, the per-namespace post-commit registry
	// (notify.go, §9.3) — its own mutex, never s.mu: write-path dispatch
	// must not contend with namespace open/evict, and a listener's fn runs
	// outside every lock.
	notifyMu  sync.Mutex
	listeners map[string][]*commitListener

	// changeRetention is the change log's retention bound R (§9.3): the
	// shared knob for cursor-token expiry and record pruning, fixed at Open —
	// it shapes durable state (what resolve honors, what prune deletes), so
	// it is not mid-life mutable. <= 0 disables both, an operator's disk
	// choice, never a correctness requirement.
	changeRetention time.Duration
}

type nsDB struct {
	rw *sql.DB
	ro *sql.DB
}

// OpenOption customizes a Store at open time.
type OpenOption func(*Store)

// WithChangeRetention sets the change log's retention bound R (§9.3):
// cursor tokens expire by their issuance + R and records are pruned past the
// 2R hold when no live page chain can reach them. 0 (or negative) disables
// both — records accumulate and cursors never expire. The deployment-side
// validation (0 or 1h–2160h) belongs to the binary's config layer; the engine
// accepts whatever it is handed.
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
	return s, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var first error
	for name, n := range s.nss {
		if err := n.rw.Close(); err != nil && first == nil {
			first = err
		}
		if err := n.ro.Close(); err != nil && first == nil {
			first = err
		}
		delete(s.nss, name)
	}
	return first
}

// ns opens the namespace's databases. It never creates: a namespace exists
// when its file does (CreateNamespace is the only creation path), and a
// missing one is ErrNotFound — the §6.2 global rule that engines never
// create implicitly, literal since slice 2b (previously the first use of a
// name silently materialized an empty namespace). The registry DDL below is
// idempotent, so opening a file that exists but predates a registry table
// still upgrades it in place.
func (s *Store) ns(name string) (*nsDB, error) {
	if err := validateNSPath(name); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lockedNS(name)
}

// nsCtx is ns for callers whose context bounds the registry wait: the
// acquire is context-aware (ctxMutex) and a cache-missing first open
// carries the context through its initialization (lockedNSCtx), so a
// bounded read — wait_for's poll loop — aborts at its deadline instead of
// queueing behind a long holder (a drop_namespace draining its pools) or
// waiting out SQLite's busy_timeout behind another process's write lock,
// where a plain mutex wait or context-free SQL would sit past every bound.
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

// lockedNS is ns() for a caller already holding s.mu: CreateNamespace
// reserves, evicts, and initializes under one lock span, so a concurrent
// first-use open can never interleave with them.
func (s *Store) lockedNS(name string) (*nsDB, error) {
	return s.lockedNSCtx(context.Background(), name)
}

// lockedNSCtx is lockedNS with the caller's context carried through the
// FIRST-OPEN initialization: the registry DDL and the nsgen transaction use
// the context-aware SQL methods, so a bounded read that misses the cache
// (the first wait_for after a restart) aborts at its deadline instead of
// waiting out the DSN's busy_timeout behind another process's write lock —
// the filesystem steps (Lstat, Chmod) have no wait to honor.
func (s *Store) lockedNSCtx(ctx context.Context, name string) (*nsDB, error) {
	if n, ok := s.nss[name]; ok {
		return n, nil
	}
	if err := s.verifyNSDirs(name); err != nil {
		return nil, err
	}
	path := s.nsPath(name)
	// Lstat, not Stat, and a regular file or nothing: a symlink at the
	// namespace's own name is not a namespace, and opening one would read
	// and write whatever it points at, outside s.dir.
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: namespace %s", ErrNotFound, name)
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
	for _, ddl := range registryDDL {
		if _, err := rw.ExecContext(ctx, ddl); err != nil {
			rw.Close()
			return nil, fmt.Errorf("init namespace %s: %w", name, err)
		}
	}
	// The creation id is minted as part of this same init path, so a
	// namespace file that predates _dolmen_meta (see the comment above) is
	// upgraded in place with a fresh id, and every later read —
	// NamespaceState, TableState — may assume the row exists.
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
	// _dolmen_drop_gen counts drops per table name and survives recreation:
	// writers compare it around their embedding pause so a write validated
	// against a dropped table cannot commit into a same-named successor
	// (whose version-1 schema the version compare alone cannot distinguish
	// from the original's). See tableGen and DropTable.
	`CREATE TABLE IF NOT EXISTS _dolmen_drop_gen(
		table_name TEXT PRIMARY KEY,
		gen INTEGER NOT NULL
	)`,
	// _dolmen_meta holds the namespace's singleton values. nsgen is the
	// namespace's creation id (§3.4): 16 crypto-random bytes minted once at
	// first init (ensureNSGen) and immutable for the namespace's lifetime —
	// DropNamespace deletes it with the file, so a recreated namespace mints
	// a fresh one and an id never repeats across namespace lifetimes.
	`CREATE TABLE IF NOT EXISTS _dolmen_meta(
		key TEXT PRIMARY KEY,
		value BLOB
	)`,
	// _dolmen_changes is the durable per-namespace change log (§9.3): one row
	// per affected id, minted INSIDE the write transaction it describes
	// (mintChanges), so there is no crash window in which an acknowledged
	// write is missing from the log. The AUTOINCREMENT seq IS the
	// per-namespace cursor — monotonic and gap-free by construction:
	// sqlite_sequence updates are transactional (a rolled-back mint restores
	// the counter) and the immediate write lock serializes writers, so one
	// transaction's rows are always contiguous. nsgen + drop_gen are the
	// lifetime labels (§3.4): the namespace's creation id and the table's
	// current drop generation, so replay can select a table's CURRENT
	// lifetime and a drop-and-recreated successor never inherits its
	// predecessor's feed. owner is the row's authorization label (§9.3) —
	// NULL until owner stamping lands (slice 9c); the column exists now so
	// enabling it later needs no registry rebuild. DropTable does not purge
	// these rows (lifetime labels, not deletion — §3.4/D24); DropNamespace
	// deletes the log with the file, free.
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
	// _dolmen_cursor_tokens is the coordinated-storage cursor mapping (§9.3):
	// a client-facing cursor token is a random string, and THIS row — not any
	// position-derived encoding — is what it maps back to. The client never
	// sees position-derived bytes, so the opacity rules hold trivially: no
	// order to reveal (a filtered feed's gaps stay invisible), no equality to
	// compare across polls (every issuance is fresh randomness; an EMPTY page
	// is not an issuance — ChangesSince returns the caller's own token
	// unchanged, or a 250 ms poll would mint a durable row every tick), no
	// length to grow at an encoding boundary. Living in the namespace db makes
	// the mapping durable across restarts (a restart must not invalidate
	// clients' cursors) and shared by every process on the deployment, and
	// its namespace-lifetime binding is inherent — it dies with the file
	// (§5.4), so a recreated namespace's restarted sequence can never be
	// silently skipped by a predecessor's token.
	//
	// Columns: position is the log seq the token resumes AFTER (0 = before
	// everything, head = future commits only). issued_at (unix ms) anchors the
	// per-token deadline, now ≤ issued_at + R. feed_table binds the token to
	// the feed it was minted on ('' = the unfiltered namespace feed); resolve
	// rejects cross-feed reuse instead of honoring a foreign position, which
	// would silently skip the other feed's events. The chain_* columns carry
	// the page chain: chain_id groups a begin/head start and every next-page
	// token it mints; chain_origin is the chain's fixed resume origin — the
	// begin-boundary semantics preserved unchanged across the chain; and
	// chain_start (unix ms) is set once at chain creation and inherited by
	// every token in the chain, anchoring the absolute cap chain_start + 2R.
	// The cap reads chain_start, never the oldest surviving token row —
	// pruning removes old rows, and a cap derived from them would drift with
	// pruning, letting repeated paging retain an old backlog past the
	// retention bound.
	`CREATE TABLE IF NOT EXISTS _dolmen_cursor_tokens(
		token TEXT PRIMARY KEY,
		position INTEGER NOT NULL,
		issued_at INTEGER NOT NULL,
		chain_id TEXT NOT NULL,
		chain_origin INTEGER NOT NULL,
		chain_start INTEGER NOT NULL,
		feed_table TEXT NOT NULL DEFAULT ''
	)`,
	// _dolmen_changes_table_feed serves ChangesSince's table-filtered page
	// read (slice 5c): the lifetime equality (table_name, drop_gen, nsgen)
	// with seq ordered inside it. Without it the lookup rides the seq
	// primary key past every unrelated table's records — a quiet table
	// polled in a busy namespace rescans an ever-growing tail of foreign
	// changes inside the namespace's single write transaction.
	`CREATE INDEX IF NOT EXISTS _dolmen_changes_table_feed
		ON _dolmen_changes(table_name, drop_gen, nsgen, seq)`,
	// _dolmen_changes_at serves the retention prune's age predicate: a log
	// whose records are all younger than the 2R hold is the common case, and
	// the prune runs on every changes_since call — an unindexed `at < ?`
	// would full-scan the log and delete nothing, under the write lock.
	`CREATE INDEX IF NOT EXISTS _dolmen_changes_at ON _dolmen_changes(at)`,
	// The cursor-token prune's predicates (issued_at, chain_start) and the
	// reach boundary MIN(chain_origin): tokens accumulate one-plus per poll
	// for a whole retention window, and every changes_since call prunes —
	// without these indexes each call full-scans the whole token table.
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
		// rw, not SQLite's default rwc: ns() checks the file exists before
		// opening, but the writable pool's connections are established lazily —
		// a namespace dropped between the stat and the first use would
		// otherwise be silently recreated by an rwc open (and the registry DDL
		// would materialize it), violating the no-implicit-creation contract.
		// rw makes the open itself fail instead.
		q.Add("mode", "rw")
		q.Add("_pragma", "journal_mode(WAL)")
		q.Add("_pragma", "synchronous(NORMAL)")
		// Writes take the lock at BEGIN, not at the first write: with a
		// deferred transaction, a writer in another process that commits
		// between our reads and our first write fails with SQLITE_BUSY_SNAPSHOT
		// (not retried by busy_timeout). Immediate locking serializes writers
		// up front, so read-then-write plans — like an idempotent insert's
		// key lookup followed by its row inserts — see a stable snapshot and
		// cross-process retries dedupe instead of erroring.
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
	// Do not validate the table name here. Existing tables created before the
	// keyword restriction (or with legacy names) must remain loadable so that
	// describe, insert, search, update, delete, and migrate keep working.
	// CreateTable still enforces ValidateTableName for new tables.
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
	// UseNumber keeps declared numeric defaults exact across the schema_json
	// round-trip: a number field's default above JSON's safe-integer range must
	// survive to later inserts and describe_table, not collapse to the nearest
	// float64.
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

// ListTables lists the namespace's tables (§6.2). TODO(8c): bindings are
// ignored while auth is off — slice 8c verifies them atomically with the
// listing.
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

// rowsQuerier is the read shape shared by *sql.DB and *sql.Tx so registry
// reads and statement execution can share one snapshot.
type rowsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// registeredTables returns the names recorded in the namespace's table
// registry, so the query guard can allow tables whose names predate current
// reservation rules (e.g. pragma_* or dbstat) while still rejecting the
// internal tables those names collide with.
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

// DescribeTable returns the table's schema and row count (§4.3). TODO(9d):
// scope and scopeIncarnation are ignored while auth is off — a non-nil scope
// will bound the count.
func (s *Store) DescribeTable(ctx context.Context, nsName, table string, scope *RowScope, scopeIncarnation Incarnation) (*schema.TableSchema, int64, error) {
	n, err := s.ns(nsName)
	if err != nil {
		return nil, 0, err
	}
	sc, err := loadSchema(ctx, n.ro, nsName, table)
	if err != nil {
		return nil, 0, err
	}
	var count int64
	if err := n.ro.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT count(*) FROM %s`, q(table))).Scan(&count); err != nil {
		return nil, 0, err
	}
	return sc, count, nil
}

// Migration is one recorded schema transition from _dolmen_migrations.
type Migration struct {
	ID          int64           `json:"id"`
	FromVersion int             `json:"from_version"`
	ToVersion   int             `json:"to_version"`
	Changes     []schema.Change `json:"changes"`
	At          string          `json:"at"`
}

// ListMigrations returns a table's migration history, newest first, with each
// transition's recorded changes decoded. Creating the table is version 1 and is
// not part of the log, so the newest entry's to_version is the current version.
// TODO(8c): inc is ignored while auth is off — slice 8c verifies the
// table-lifetime guard atomically with the read.
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
		// UseNumber keeps recorded numeric defaults exact: a default above
		// JSON's safe-integer range must survive the audit round-trip, not
		// collapse to the nearest float64.
		dec := json.NewDecoder(strings.NewReader(cj))
		dec.UseNumber()
		if err := dec.Decode(&m.Changes); err != nil {
			return nil, fmt.Errorf("corrupt migration record %d for %s.%s: %w", m.ID, nsName, table, err)
		}
		// Histories written before value became set_*-only still carry an
		// inert "value": false on non-flag changes. Drop it on read so every
		// entry matches the current contract and stays replayable through
		// migrate, which now rejects values on non-flag ops.
		for j := range m.Changes {
			if m.Changes[j].Op != schema.OpSetFulltext && m.Changes[j].Op != schema.OpSetVectorize {
				m.Changes[j].Value = nil
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

const MaxFieldsPerTable = 100

// CreateTable creates a table (§6.2). TODO(8c): nsGen is ignored while auth
// is off — slice 8c verifies the namespace exists with exactly that creation
// id inside the operation's critical section. TODO(9a): opts.RowAccess is
// ignored until row access lands.
func (s *Store) CreateTable(ctx context.Context, nsName, table string, fields []schema.Field, opts TableOpts, nsGen [16]byte) (*schema.TableSchema, error) {
	if err := schema.ValidateTableName(table); err != nil {
		return nil, invalidf("%s", err)
	}
	if len(fields) > MaxFieldsPerTable {
		return nil, invalidf("too many fields: %d (max %d; SQLite caps tables at 2000 columns including the implicit id, created_at, and _embedding)", len(fields), MaxFieldsPerTable)
	}
	fields = schema.Normalize(fields)
	if err := schema.Validate(fields); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := validateFieldDefaults(fields); err != nil {
		return nil, err
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
	if _, err := tx.ExecContext(ctx, tableDDL(table, fields)); err != nil {
		return nil, err
	}
	if fts := ftsFields(fields); len(fts) > 0 {
		if err := createFTS(ctx, tx, table, fts); err != nil {
			return nil, err
		}
	}
	sc := &schema.TableSchema{Namespace: nsName, Name: table, Version: 1, Fields: fields}
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

// validateFieldDefaults checks that each declared default coerces through its
// field's type at create time, so a mismatched default fails here instead of
// at the first insert that omits the field. The raw declared value is what the
// schema persists; inserts coerce it through the same path on every use.
func validateFieldDefaults(fields []schema.Field) error {
	for _, f := range fields {
		if f.Default == nil {
			continue
		}
		cv, err := coerceValue(f, f.Default)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		// Non-finite floats cannot arrive through JSON but can through direct
		// store use; they have no honest stored meaning — reject them outright.
		if fv, isFloat := cv.(float64); isFloat && (math.IsNaN(fv) || math.IsInf(fv, 0)) {
			return invalidf("field %q: default must be a finite number", f.Name)
		}
		// Same rule as add_field defaults: a NUL byte cannot appear in stored
		// SQL text and would confuse FTS — reject it regardless of how the
		// default was declared.
		if sv, isStr := cv.(string); isStr && strings.ContainsRune(sv, 0) {
			return invalidf("field %q: default must not contain NUL bytes", f.Name)
		}
	}
	return nil
}

func tableDDL(table string, fields []schema.Field) string {
	var sb strings.Builder
	// AUTOINCREMENT keeps ids monotonic and never reused: without it SQLite
	// assigns max(id)+1, so deleting every row lets fresh rows collide with
	// ids agents may still be keying off.
	sb.WriteString(fmt.Sprintf(`CREATE TABLE %s (id INTEGER PRIMARY KEY AUTOINCREMENT, created_at TEXT NOT NULL DEFAULT (strftime('%%Y-%%m-%%dT%%H:%%M:%%fZ','now'))`, q(table)))
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

// createFTS builds the shadow FTS5 index with the porter stemming wrapper over
// unicode61: case and diacritic folding are unchanged, while English suffixes
// collapse to stems so inflected queries ("payments") match indexed forms
// ("payment") without a prefix wildcard. Stemming applies to queries and the
// index alike — phrases and prefix terms operate on stems, and it is
// English-focused (CJK runs pass through untouched; see #106).
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
