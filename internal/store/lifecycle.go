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

func (s *Store) ListNamespaces(ctx context.Context, prefix string, bindings []AuthBinding) ([]string, error) {
	if s.closed.Load() {
		return nil, ErrClosed
	}
	var out []string
	root, at := s.dir, ""
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

func sortNS(nss []string) {
	sort.Slice(nss, func(i, j int) bool { return nss[i]+".db" < nss[j]+".db" })
}

func (s *Store) nsSubtree(prefix string) (self bool, dir string, err error) {
	segs := strings.Split(prefix, "/")

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

func walkNamespaces(dir, prefix string, out *[]string) error {

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

func (s *Store) CreateNamespace(ctx context.Context, nsName string, parentNsGen [16]byte) error {
	if err := validateNSPath(nsName); err != nil {
		return err
	}
	path := s.nsPath(nsName)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return ErrClosed
	}

	if err := s.verifyNSDirs(nsName); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%w: namespace %s %w", ErrInvalid, nsName, ErrExists)
		}
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}

	s.evict(nsName)
	s.endListenSessions(nsName, ErrListenLifetimeEnded)
	if _, err := s.lockedNS(nsName); err != nil {

		_ = os.Remove(path)
		return err
	}
	return nil
}

func (s *Store) DropNamespace(ctx context.Context, nsName string, nsGen [16]byte) error {
	if err := validateNSPath(nsName); err != nil {
		return err
	}
	path := s.nsPath(nsName)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return ErrClosed
	}
	if err := s.verifyNSDirs(nsName); err != nil {
		return err
	}

	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {

			s.evict(nsName)
			s.endListenSessions(nsName, ErrListenLifetimeEnded)
			return fmt.Errorf("%w: namespace %s does not exist, so nothing was dropped; list_namespaces shows what is there", ErrNotFound, nsName)
		}
		return err
	}
	if !fi.Mode().IsRegular() {
		return invalidf("namespace %s: %s is not a regular file", nsName, path)
	}

	if n, err := s.descendants(ctx, nsName); err != nil {
		return err
	} else if n > 0 {
		what := "descendant namespaces"
		if n == 1 {
			what = "descendant namespace"
		}
		return invalidf("namespace %s has %d %s — drop the children first", nsName, n, what)
	}

	s.evict(nsName)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		s.endListenSessions(nsName, fmt.Errorf("listen: namespace %s evicted but its drop failed: %w", nsName, err))
		return fmt.Errorf("drop namespace %s: %w", nsName, err)
	}
	s.endListenSessions(nsName, ErrListenLifetimeEnded)
	for _, p := range []string{path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("drop namespace %s: %w", nsName, err)
		}
	}
	return nil
}

func (s *Store) descendants(ctx context.Context, nsName string) (int, error) {
	nss, err := s.ListNamespaces(ctx, nsName, nil)
	if err != nil {
		return 0, err
	}
	n := len(nss)
	if n > 0 && nss[0] == nsName {
		n--
	}
	return n, nil
}

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

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO _dolmen_drop_gen(table_name, gen) VALUES(?, 1)
		 ON CONFLICT(table_name) DO UPDATE SET gen = gen + 1`, table); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.wakeListenSessions(nsName)
	return nil
}

func (s *Store) TableState(ctx context.Context, nsName, table string, auth []AuthBinding) (*schema.TableSchema, Incarnation, error) {
	n, err := s.ns(nsName)
	if err != nil {
		return nil, Incarnation{}, err
	}

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

func (s *Store) evict(name string) {
	n, ok := s.nss[name]
	if !ok {
		return
	}

	_ = n.ro.Close()
	_ = n.rw.Close()
	for n.ro.Stats().OpenConnections > 0 || n.rw.Stats().OpenConnections > 0 {
		time.Sleep(2 * time.Millisecond)
	}
	delete(s.nss, name)
}

func validateNSPath(name string) error {
	return ValidateNamespace(name)
}

func ValidateNamespace(name string) error {
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

			return invalidf("invalid namespace %q: %s is not a directory", name, cur)
		}
	}
	return nil
}

func (s *Store) nsPath(name string) string {
	segs := strings.Split(name, "/")
	segs[len(segs)-1] += ".db"
	return filepath.Join(append([]string{s.dir}, segs...)...)
}
