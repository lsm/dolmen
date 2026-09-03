package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ListNamespaces returns the namespaces that exist under the data directory,
// sorted by name. A namespace exists when its <name>.db file does; files whose
// stem could not be a valid namespace name are skipped, so the list matches
// exactly what the store can open.
func (s *Store) ListNamespaces() ([]string, error) {
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
		if name == e.Name() || !nsRe.MatchString(name) {
			continue
		}
		out = append(out, name)
	}
	// os.ReadDir already orders entries by filename.
	return out, nil
}

// CreateNamespace creates an empty namespace — its SQLite file plus registry
// tables — up front. Namespaces are otherwise created implicitly on first use;
// this exists so callers can reserve a name deliberately, and it fails when
// the namespace already exists. Reservation is atomic (O_EXCL): exactly one
// concurrent or cross-process caller wins the name.
func (s *Store) CreateNamespace(nsName string) error {
	if err := validateNS(nsName); err != nil {
		return err
	}
	path := s.nsPath(nsName)
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
	// it so ns() initializes the fresh file instead of serving dead pools.
	s.mu.Lock()
	s.evict(nsName)
	s.mu.Unlock()
	if _, err := s.ns(nsName); err != nil {
		// Un-reserve so a failed init doesn't wedge the name behind a
		// zero-byte file.
		_ = os.Remove(path)
		return err
	}
	return nil
}

// DropNamespace removes a namespace and every table in it: cached connections
// are closed and evicted, then the SQLite file and its WAL sidecars are
// deleted. Close waits for in-flight requests on that namespace to finish;
// requests that arrive after the drop fail or — like any later use of the
// name — recreate the namespace empty.
//
// The caveat from the stop-server-and-delete era still applies across
// processes: another process holding the namespace file open (a second
// dolmen, a backup tool, a sqlite shell) is not detected. Coordinate drops
// within one server.
func (s *Store) DropNamespace(nsName string) error {
	if err := validateNS(nsName); err != nil {
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
func (s *Store) DropTable(ctx context.Context, nsName, table string) error {
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

// evict closes a namespace's cached pools and removes them from the cache.
// Callers must hold s.mu. ro closes before rw so rw's final close is the one
// that checkpoints the WAL. Close errors are advisory — the caller is
// discarding the namespace either way.
func (s *Store) evict(name string) {
	if n, ok := s.nss[name]; ok {
		_ = n.ro.Close()
		_ = n.rw.Close()
		delete(s.nss, name)
	}
}

func validateNS(name string) error {
	if !nsRe.MatchString(name) {
		return invalidf("invalid namespace %q: must match ^[a-z0-9][a-z0-9_-]{0,63}$", name)
	}
	return nil
}

func (s *Store) nsPath(name string) string {
	return filepath.Join(s.dir, name+".db")
}
