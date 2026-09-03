package store

import (
	"context"
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
// the namespace already exists.
func (s *Store) CreateNamespace(nsName string) error {
	if err := validateNS(nsName); err != nil {
		return err
	}
	if _, err := os.Stat(s.nsPath(nsName)); err == nil {
		return invalidf("namespace %s already exists", nsName)
	}
	_, err := s.ns(nsName)
	return err
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
			// Evict any stale cache entry too (its file was removed out-of-band).
			delete(s.nss, nsName)
			return fmt.Errorf("%w: namespace %s", ErrNotFound, nsName)
		}
		return err
	}
	if n, ok := s.nss[nsName]; ok {
		// ro first: the rw connection is the one that checkpoints and clears
		// the WAL on its final close. Close errors are advisory here — the
		// file removal below is the outcome that matters.
		_ = n.ro.Close()
		_ = n.rw.Close()
		delete(s.nss, nsName)
	}
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
	return tx.Commit()
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
