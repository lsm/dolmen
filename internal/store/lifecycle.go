package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lsm/dolmen/internal/schema"
)

// ListNamespaces returns every namespace under the data directory, the
// whole tree (§5.3): depth-1 names and nested a/b/c paths alike, in
// database-filename order (sortNS) — on a depth-1-only store, exactly
// v0.2.0's listing, byte for byte. A non-empty prefix (a valid namespace
// path) restricts the listing to that path's subtree, the prefix itself
// included when it names a namespace; the walk then touches only that
// path, never a sibling, so an unreadable or huge unrelated subtree can
// neither fail nor slow it — and neither can DropNamespace's descendant
// check, which counts through the same listing. The empty prefix lists
// everything. The list names exactly the namespaces the store can open:
// entries whose stem is not a valid namespace segment are skipped, as is
// anything but a regular file (a symlink named like a namespace database
// is one of verifyNSDirs' refusals).
// TODO(8c): bindings are ignored while auth is off.
func (s *Store) ListNamespaces(ctx context.Context, prefix string, bindings []AuthBinding) ([]string, error) {
	var out []string
	root, at := s.dir, "" // the full walk: everything under the data directory
	if prefix != "" {
		if err := validateNSPath(prefix); err != nil {
			return nil, err
		}
		self, dir, err := s.nsSubtree(prefix)
		if err != nil {
			return nil, err
		}
		if self {
			out = append(out, prefix)
		}
		if dir == "" {
			// No subtree directory: the listing is the prefix's own file,
			// if it was one.
			sortNS(out)
			return out, nil
		}
		root, at = dir, prefix
	}
	if err := walkNamespaces(root, at, &out); err != nil {
		return nil, err
	}
	sortNS(out)
	return out, nil
}

// sortNS orders a namespace listing the way v0.2.0's os.ReadDir ordered a
// depth-1 store — by database filename, stem+".db", generalized to full
// paths (§5.3's depth-1 byte-identity, §8.1). Sorting the bare names is
// NOT the same order and would break existing stores: v0.2.0 lists
// a-foo.db before a.db ('-' sorts before '.'), so "a-foo" precedes "a",
// while as bare paths "a" is a prefix of "a-foo" and would flip first.
func sortNS(nss []string) {
	sort.Slice(nss, func(i, j int) bool { return nss[i]+".db" < nss[j]+".db" })
}

// nsSubtree resolves a prefix into its listing inputs: whether the prefix
// itself is a namespace (its <seg>.db is a regular file in its parent
// directory), and the directory its descendants live under (<data>/a/b/c
// for a/b/c), "" when that directory does not exist. The component chain
// is walked one directory at a time — every stat lands on a path whose
// parents are already verified real directories — and stops at the first
// component that is missing or not a real directory: a symlink is never
// followed (verifyNSDirs refuses opens through one, and the walk's IsDir
// rule descends through none), so a subtree through it lists empty rather
// than reading outside the data directory, and a file sitting where a
// directory component should be hides the rest of the path instead of
// erroring. A stat error that is not plain absence — an inaccessible
// component — propagates instead: the subtree's namespaces are unknowable,
// and a silent empty listing would report them as absent, which the walk
// never does (it propagates ReadDir errors for the same reason).
func (s *Store) nsSubtree(prefix string) (self bool, dir string, err error) {
	segs := strings.Split(prefix, "/")
	// statComponent is one link of the chain: isDir=false with a nil error
	// means the component is missing or not a directory — either way
	// nothing below it can be reached; a non-nil error is real I/O.
	statComponent := func(path string) (bool, error) {
		fi, statErr := os.Lstat(path)
		if os.IsNotExist(statErr) {
			return false, nil
		}
		if statErr != nil {
			return false, statErr
		}
		return fi.IsDir(), nil
	}
	chain := s.dir
	for _, seg := range segs[:len(segs)-1] {
		chain = filepath.Join(chain, seg)
		isDir, err := statComponent(chain)
		if err != nil {
			return false, "", err
		}
		if !isDir {
			return false, "", nil
		}
	}
	// The prefix's own file: only its absence or regularity matters.
	if fi, statErr := os.Lstat(filepath.Join(chain, segs[len(segs)-1]+".db")); statErr == nil && fi.Mode().IsRegular() {
		self = true
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return false, "", statErr
	}
	dir = filepath.Join(chain, segs[len(segs)-1])
	isDir, err := statComponent(dir)
	if err != nil {
		return self, "", err
	}
	if !isDir {
		return self, "", nil
	}
	return self, dir, nil
}

// walkNamespaces collects the namespaces of one directory subtree into out:
// every regular <segment>.db file is a namespace, and descent continues
// through directories named for a valid segment — only while another
// segment still fits under §5.1's depth cap: a directory AT the cap
// (<data>/a/b/c/ beside a/b/c.db) can hold nothing the grammar can name,
// so the walk does not open it at all and an unreadable or huge one can
// neither fail nor slow the listing. Descent is never through anything but
// a real directory (DirEntry.IsDir is lstat semantics: a symlink reports
// as a link and is not descended, so the walk stays inside s.dir the way
// verifyNSDirs keeps opens there). dir must be a directory inside s.dir;
// prefix is the namespace path of dir's contents — "" at the
// data-directory root, or a subtree root, which may sit AT the cap: only
// the prefix's own file (checked by the caller) is a namespace there, so
// the walk reports nothing and descends no further. The caller orders the
// collected names (sortNS): ReadDir's per-directory filename order is not
// a whole-tree order (a/b's b.db is collected after a/b/c's c.db, though
// "b.db" < "c.db").
func walkNamespaces(dir, prefix string, out *[]string) error {
	// depth is the segment count of prefix: a file at this level names a
	// depth+1 namespace, a directory's contents depth+2 and deeper. The
	// walk enters a directory only while those depths still fit the cap,
	// which is also what keeps every emitted path valid without a
	// per-file depth check.
	depth := 0
	if prefix != "" {
		depth = strings.Count(prefix, "/") + 1
	}
	if depth >= maxNSDepth {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	descend := depth+2 <= maxNSDepth
	for _, e := range entries {
		name := e.Name()
		// One lstat decides what the entry is (Info is lstat semantics: a
		// symlink reports the link itself, and fi.IsDir never leans on the
		// dirent type, which some filesystems leave unknown). A vanished
		// entry is just gone; any other metadata error propagates — an
		// unsearchable directory (mode r without x) hands ReadDir the
		// names but refuses the stat, and a silently skipped entry is a
		// silently missing descendant: DropNamespace counts children
		// through this walk and must fail closed, never undercount into
		// deleting a parent whose child exists.
		fi, err := e.Info()
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if fi.IsDir() {
			if !descend || !nsSegmentRe.MatchString(name) {
				continue
			}
			child := name
			if prefix != "" {
				child = prefix + "/" + name
			}
			if err := walkNamespaces(filepath.Join(dir, name), child, out); err != nil {
				return err
			}
			continue
		}
		stem := strings.TrimSuffix(name, ".db")
		if stem == name || !nsSegmentRe.MatchString(stem) {
			continue
		}
		// A listed namespace must be one the store would open, not one it
		// refuses: lockedNS's regular-file check.
		if !fi.Mode().IsRegular() {
			continue
		}
		ns := stem
		if prefix != "" {
			ns = prefix + "/" + stem
		}
		*out = append(*out, ns)
	}
	return nil
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
	// A child namespace's parent directory is created here, on first child
	// creation (§5.2) — never on open: CreateNamespace is the only creation
	// path (ns() opens, it does not create), so the first creation of a/b
	// while a exists as a.db must not depend on any other path having made
	// <data>/a/. verifyNSDirs first refuses a symlinked component, then
	// MkdirAll creates what is missing (idempotent, so concurrent creators
	// still race only on the O_EXCL open below). Depth-1 namespaces have no
	// parent directory to make beyond s.dir itself, which Open already
	// created.
	if err := s.verifyNSDirs(nsName); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
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
// empty. A namespace with descendants is not dropped: the request is refused
// (invalid) naming the descendant count — children are dropped first, never
// deleted implicitly (§5.4).
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
	if err := s.verifyNSDirs(nsName); err != nil {
		return err
	}
	// Lstat, not Stat, and a regular file or nothing: a symlink at the
	// namespace's own name is not a namespace (opening one would read and
	// write through it), and the removal below must be reachable only for a
	// file the store itself created — the symlinked-parent case that would
	// delete an external file is refused by verifyNSDirs above.
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			// The file was removed out-of-band (or never existed): close the
			// stale cached pools rather than orphaning them — Close() only
			// reaches entries still in the map.
			s.evict(nsName)
			return fmt.Errorf("%w: namespace %s", ErrNotFound, nsName)
		}
		return err
	}
	if !fi.Mode().IsRegular() {
		return invalidf("namespace %s: %s is not a regular file", nsName, path)
	}
	// Leaf-only (§5.4): a drop never deletes a subtree by accident. A
	// namespace with descendants is refused — the error naming the
	// descendant count, before anything is evicted or deleted. The count
	// runs under s.mu, so within one server a concurrent child creation
	// cannot slip between it and the deletion (another process can — the
	// standing cross-process caveat above).
	if n, err := s.descendants(ctx, nsName); err != nil {
		return err
	} else if n > 0 {
		what := "descendant namespaces"
		if n == 1 {
			what = "descendant namespace"
		}
		return invalidf("namespace %s has %d %s — drop the children first", nsName, n, what)
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

// descendants counts the namespaces strictly below nsName — its subtree
// minus itself — the §5.4 leaf-only drop guard's number. Callers have
// already validated the path and established the namespace's file exists;
// the count is the listing's subtree read, so it counts exactly what a
// re-list would report, and it fails closed: any error reading the
// subtree aborts the drop rather than undercounting children away. Under
// sortNS a prefix's own file orders before every extension of it
// ("a/b.db" < "a/b/x.db": '.' < '/'), so nsName is the first entry
// whenever it is a namespace at all.
func (s *Store) descendants(ctx context.Context, nsName string) (int, error) {
	nss, err := s.ListNamespaces(ctx, nsName, nil)
	if err != nil {
		return 0, err
	}
	n := len(nss)
	if n > 0 && nss[0] == nsName {
		n-- // the subtree includes the prefix itself
	}
	return n, nil
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
// auth is off.
func (s *Store) TableState(ctx context.Context, nsName, table string, auth []AuthBinding) (*schema.TableSchema, Incarnation, error) {
	n, err := s.ns(nsName)
	if err != nil {
		return nil, Incarnation{}, err
	}
	// One read snapshot for the schema, the drop generation, and the creation
	// id: two autocommit reads could straddle a concurrent drop + same-name
	// recreate and pair the predecessor's schema with the successor's DropGen
	// — the snapshot this read hands out must describe exactly one table
	// lifetime, of exactly one namespace lifetime.
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
	gen, err := readNSGen(ctx, tx)
	if err != nil {
		return nil, Incarnation{}, err
	}
	return sc, Incarnation{NsGen: gen, Table: table, Version: int64(sc.Version), DropGen: dropGen}, nil
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
	if len(segs) > maxNSDepth {
		return invalidf("invalid namespace %q: namespace paths are 1-3 segments (a/b/c), got %d", name, len(segs))
	}
	for _, seg := range segs {
		if !nsSegmentRe.MatchString(seg) {
			return invalidf("invalid namespace %q: every segment must match ^[a-z0-9][a-z0-9_-]{0,63}$ (no empty segments — leading, trailing, or double slashes)", name)
		}
	}
	return nil
}

// verifyNSDirs enforces filesystem containment for a namespace path: every
// directory component below s.dir — <data>/a, <data>/a/b for a/b/c — must be
// a real directory, never a symlink or anything else. validateNSPath bars
// "." and "/" lexically; this bars them physically: a planted `a ->
// /outside` inside the data directory would otherwise let namespace I/O
// follow it out of s.dir — CreateNamespace writing, ns() reading, and
// DropNamespace removing files the store never owned. Components that do
// not exist yet are fine: nothing can exist below a missing component, and
// the caller either creates them (CreateNamespace's MkdirAll) or reports
// ErrNotFound on the file itself.
//
// This is one walk, not a held guard: a same-UID process racing the data
// directory can swap a verified component for a symlink before the caller's
// next filesystem call. That race lives under the store's standing
// cross-process caveat (DropNamespace's doc) — namespace mutation is
// coordinated within one server, and the directory's contents are trusted
// between operations, never merely at check time. No-follow fd-relative
// traversal here would not change that: SQLite's VFS opens the database by
// path on every pooled connection, so the read/write path cannot be closed
// at this call site. What the walk buys is the static case, deterministically:
// a pre-existing symlinked layout fails loudly instead of being followed.
func (s *Store) verifyNSDirs(name string) error {
	segs := strings.Split(name, "/")
	cur := s.dir
	for _, seg := range segs[:len(segs)-1] {
		cur = filepath.Join(cur, seg)
		fi, err := os.Lstat(cur)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			// Lstat on a symlink-to-directory reports the link, not the
			// directory, so this catches it.
			return invalidf("invalid namespace %q: %s is not a directory", name, cur)
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
