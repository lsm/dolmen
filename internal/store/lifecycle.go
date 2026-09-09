package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lsm/dolmen/internal/schema"
)

// ListNamespaces returns the namespaces that exist under the data directory,
// sorted by name. Listing is still depth-1 (TODO(3b)): only top-level
// <name>.db files are reported — a nested namespace's b.db lives inside the
// a/ directory (slice 3a's layout) and stays invisible until 3b's recursive
// walk, which is also when prefix starts filtering to a subtree. Top-level
// files whose stem is not a valid namespace segment are skipped, so the list
// matches exactly what the store can open at depth 1.
// TODO(8c): bindings are ignored while auth is off.
func (s *Store) ListNamespaces(ctx context.Context, prefix string, bindings []AuthBinding) ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".db")
		if name == e.Name() || !nsSegmentRe.MatchString(name) {
			continue
		}
		out = append(out, name)
	}
	// os.ReadDir already orders entries by filename.
	return out, nil
}

// CreateNamespace creates an empty namespace — its SQLite file plus registry
// tables — up front. It is the only creation path (nothing creates a
// namespace implicitly since slice 2b), it exists so callers can reserve a
// name deliberately, and it fails when the namespace already exists.
// Reservation is atomic (O_EXCL): exactly one concurrent or cross-process
// caller wins the name.
// TODO(8c): parentNsGen is ignored while auth is off — slice 8c verifies the
// parent's incarnation atomically with creation.
func (s *Store) CreateNamespace(ctx context.Context, nsName string, parentNsGen [16]byte) error {
	if err := validateNSPath(nsName); err != nil {
		return err
	}
	path := s.nsPath(nsName)
	// A child namespace's parent directory is created here, on first child
	// creation (§5.2) — never on open: CreateNamespace is the only creation
	// path (ns() opens, it does not create), so the first creation of a/b
	// while a exists as a.db must not depend on any other path having made
	// <data>/a/. MkdirAll is idempotent, so concurrent creators still race
	// only on the O_EXCL open below. Depth-1 namespaces have no parent
	// directory to make beyond s.dir itself, which Open already created.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Reservation, eviction, and initialization hold one lock span. The O_EXCL
	// loser of a concurrent CreateNamespace returns already-exists and
	// proceeds straight to ns() (the op layer's ensureNamespace treats
	// already-exists as success); an open that slipped between this
	// reservation and the evict would cache pools the evict then closes
	// underneath its in-flight request — sql: database is closed, a 500.
	// Under the lock the loser's ns() can only observe the finished namespace
	// through the cache. Another process cannot be coordinated (the
	// DropNamespace caveat): its reservation cannot evict this process's
	// pools, and SQLite's own locking serializes the registry DDL.
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return invalidf("namespace %s already exists", nsName)
		}
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	// A cached entry here is stale (its file was removed out-of-band); evict
	// it so lockedNS initializes the fresh file instead of serving dead pools.
	s.evict(nsName)
	if _, err := s.lockedNS(nsName); err != nil {
		// Un-reserve so a failed init doesn't wedge the name behind a
		// zero-byte file.
		_ = os.Remove(path)
		return err
	}
	return nil
}

// DropNamespace removes a namespace and every table in it: cached connections
// are closed, drained to zero, and evicted, then the SQLite file and its WAL
// sidecars are deleted. The drop waits for in-flight requests on the
// namespace to finish (unboundedly — see evict); requests that arrive after
// the drop fail or — like any later use of the name — recreate the namespace
// empty.
//
// The caveat from the stop-server-and-delete era still applies across
// processes: another process holding the namespace file open (a second
// dolmen, a backup tool, a sqlite shell) is not detected. Coordinate drops
// within one server.
// TODO(8c): nsGen is ignored while auth is off — slice 8c verifies the
// namespace-lifetime guard atomically with the drop.
func (s *Store) DropNamespace(ctx context.Context, nsName string, nsGen [16]byte) error {
	if err := validateNSPath(nsName); err != nil {
		return err
	}
	path := s.nsPath(nsName)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			// The file was removed out-of-band (or never existed): close the
			// stale cached pools rather than orphaning them — Close() only
			// reaches entries still in the map.
			s.evict(nsName)
			return fmt.Errorf("%w: namespace %s", ErrNotFound, nsName)
		}
		return err
	}
	// ro first: the rw connection is the one that checkpoints and clears the
	// WAL on its final close. Close errors are advisory here — the file
	// removal below is the outcome that matters.
	s.evict(nsName)
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("drop namespace %s: %w", nsName, err)
		}
	}
	return nil
}

// DropTable removes a table and everything dolmen tracks alongside it: the
// table and its rows, its full-text index, and its registry rows — schema and
// version, migration history, and idempotency keys. Purging the idempotency
// keys matters for correctness: a table recreated under the same name must
// not replay the old table's ids.
// TODO(8c): inc is ignored while auth is off — slice 8c verifies the table's
// full lifetime key atomically with the drop.
func (s *Store) DropTable(ctx context.Context, nsName, table string, inc Incarnation) error {
	n, err := s.ns(nsName)
	if err != nil {
		return err
	}
	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := loadSchema(ctx, tx, nsName, table); err != nil {
		return err
	}
	// Shadow index first (mirrors create); SQLite removes the table's
	// sqlite_sequence row together with the table itself.
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DROP TABLE IF EXISTS %s`, q(ftsTable(table)))); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DROP TABLE %s`, q(table))); err != nil {
		return err
	}
	for _, stmt := range []string{
		`DELETE FROM _dolmen_tables WHERE name = ?`,
		`DELETE FROM _dolmen_migrations WHERE table_name = ?`,
		`DELETE FROM _dolmen_idempotency WHERE table_name = ?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, table); err != nil {
			return err
		}
	}
	// Bump the persisted drop generation inside the same transaction: write
	// transactions serialize on the database's write lock, in this process or
	// any other sharing the data directory, so a writer that can observe the
	// drop's effects also observes the bump and must retry against the
	// recreated-or-absent table instead of committing a stale plan into a
	// same-named successor. A rollback undoes the bump with the drop.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO _dolmen_drop_gen(table_name, gen) VALUES(?, 1)
		 ON CONFLICT(table_name) DO UPDATE SET gen = gen + 1`, table); err != nil {
		return err
	}
	return tx.Commit()
}

// TableState is the one-snapshot read the API layer resolves scopes and
// validates text vector queries with (§6.2): the schema together with the
// Incarnation a later scoped call must pass back. The read never creates a
// namespace implicitly (see ns). TODO(8c): auth bindings are ignored while
// auth is off. TODO(4a): NsGen is zero until namespaces carry a persisted
// creation id — slice 4a's NamespaceState mints it and this read returns it.
func (s *Store) TableState(ctx context.Context, nsName, table string, auth []AuthBinding) (*schema.TableSchema, Incarnation, error) {
	n, err := s.ns(nsName)
	if err != nil {
		return nil, Incarnation{}, err
	}
	// One read snapshot for the schema and the drop generation: two autocommit
	// reads could straddle a concurrent drop + same-name recreate and pair the
	// predecessor's schema with the successor's DropGen — the snapshot this
	// read hands out must describe exactly one table lifetime.
	tx, err := n.ro.BeginTx(ctx, nil)
	if err != nil {
		return nil, Incarnation{}, err
	}
	defer tx.Rollback()
	sc, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return nil, Incarnation{}, err
	}
	dropGen, err := tableGen(ctx, tx, table)
	if err != nil {
		return nil, Incarnation{}, err
	}
	return sc, Incarnation{Table: table, Version: int64(sc.Version), DropGen: dropGen}, nil
}

// tableGen returns the persisted drop generation for a table name (0 when it
// has never been dropped). Writers read it BEFORE their pre-transaction schema
// read and again inside their write transaction: that order makes the pair
// (gen, schema) consistent-or-stale, and a drop committing anywhere in between
// is caught by the in-transaction comparison and retried.
func tableGen(ctx context.Context, db rowQuerier, table string) (int64, error) {
	var gen sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT gen FROM _dolmen_drop_gen WHERE table_name = ?`, table).Scan(&gen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return gen.Int64, nil
}

// evict closes a namespace's cached pools, waits until every connection they
// handed out has been returned and fully closed, and only then removes the
// namespace from the cache. Callers must hold s.mu, which is what keeps the
// file paths reserved for the whole drain: SQLite unlinks a WAL database's
// -wal/-shm sidecars by path on a connection's final close, so if the
// namespace were recreated (or its files deleted and re-created) while a
// straggler from the evicted pools was still alive, the straggler's unlink
// would delete the next incarnation's sidecars and its connections would
// fail with disk I/O errors. The wait is therefore unbounded: a request that
// never finishes (a hung embedding provider with no context deadline) hangs
// the eviction — and, since s.mu is held, new namespace opens — with it.
// database/sql decrements its open-connection count only after the driver's
// close returns, so observing zero means the sidecar unlinks have happened.
// Close errors are advisory — the caller is discarding the namespace anyway.
func (s *Store) evict(name string) {
	n, ok := s.nss[name]
	if !ok {
		return
	}
	// ro first: the rw connection is the one that checkpoints and clears
	// the WAL on its final close.
	_ = n.ro.Close()
	_ = n.rw.Close()
	for n.ro.Stats().OpenConnections > 0 || n.rw.Stats().OpenConnections > 0 {
		time.Sleep(2 * time.Millisecond)
	}
	delete(s.nss, name)
}

// validateNSPath enforces the §5.1 grammar: 1–3 segments matching
// nsSegmentRe, separated by single slashes. An empty segment — leading,
// trailing, or doubled slash — fails the segment match, so the split alone
// rules them out; the segment charset admits no "." or "/", so a valid path
// can never escape s.dir through the join below.
func validateNSPath(name string) error {
	segs := strings.Split(name, "/")
	if len(segs) > 3 {
		return invalidf("invalid namespace %q: namespace paths are 1-3 segments (a/b/c), got %d", name, len(segs))
	}
	for _, seg := range segs {
		if !nsSegmentRe.MatchString(seg) {
			return invalidf("invalid namespace %q: every segment must match ^[a-z0-9][a-z0-9_-]{0,63}$ (no empty segments — leading, trailing, or double slashes)", name)
		}
	}
	return nil
}

// nsPath maps a namespace path to its SQLite file — adapter #1's layout
// (§5.2): a/b/c is <data>/a/b/c.db, and a depth-1 namespace stays exactly
// v0.2.0's <data>/<name>.db. Callers validate through validateNSPath first,
// which is what keeps the join inside s.dir.
func (s *Store) nsPath(name string) string {
	segs := strings.Split(name, "/")
	segs[len(segs)-1] += ".db"
	return filepath.Join(append([]string{s.dir}, segs...)...)
}
