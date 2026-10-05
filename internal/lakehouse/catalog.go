package lakehouse

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/apache/iceberg-go"
	sqlcatalog "github.com/apache/iceberg-go/catalog/sql"
	"github.com/apache/iceberg-go/table"
	"github.com/lsm/dolmen/internal/store"
	_ "modernc.org/sqlite"
)

const catalogFormat = 2

func namespacePath(name string) string { return filepath.FromSlash(name) + ".lakehouse" }

func fileLocation(path string) string { return "file://" + filepath.ToSlash(path) }

func (s *Store) checkHierarchy(name string, create bool) error {
	parts := strings.Split(name, "/")
	path := ""
	for _, part := range parts[:len(parts)-1] {
		path = filepath.Join(path, part)
		if create {
			if err := s.root.Mkdir(path, 0o700); err == nil {
				if err := s.syncDirectory(filepath.Dir(path)); err != nil {
					return err
				}
			} else if !os.IsExist(err) {
				return err
			}
		}
		info, err := s.root.Lstat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: namespace %s has a non-directory or symlink path component", store.ErrInvalid, name)
		}
	}
	return nil
}

func (s *Store) checkNamespaceDir(name string) error {
	if err := s.checkHierarchy(name, false); err != nil {
		return err
	}
	info, err := s.root.Lstat(namespacePath(name))
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: lakehouse namespace %s is not a directory or is a symlink", store.ErrInvalid, name)
	}
	return nil
}

func (s *Store) checkNamespaceFiles(name string) error {
	if err := s.checkNamespaceDir(name); err != nil {
		return err
	}
	dir := namespacePath(name)
	for _, item := range []struct {
		path  string
		isDir bool
	}{{filepath.Join(dir, "data"), true}, {filepath.Join(dir, "catalog.db"), false}} {
		path, isDir := item.path, item.isDir
		info, err := s.root.Lstat(path)
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: lakehouse namespace %s is incomplete; use an unpinned drop_namespace to remove it before recreating it", store.ErrCatalogCorrupt, name)
		}
		if err != nil {
			return err
		}
		if isDir && !info.IsDir() || !isDir && !info.Mode().IsRegular() {
			return fmt.Errorf("%w: lakehouse namespace %s has a non-regular catalog or data directory", store.ErrInvalid, name)
		}
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		info, err := s.root.Lstat(filepath.Join(dir, "catalog.db") + suffix)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: lakehouse namespace %s has a non-regular SQLite auxiliary file", store.ErrInvalid, name)
		}
	}
	return nil
}

func (s *Store) openCatalog(ctx context.Context, name, relative string, create bool) (_ *namespace, err error) {
	path := filepath.Join(s.dir, relative, "catalog.db")
	uriPath := filepath.ToSlash(path)
	if len(uriPath) > 1 && uriPath[1] == ':' {
		uriPath = "/" + uriPath
	}
	u := &url.URL{Scheme: "file", Path: uriPath}
	q := url.Values{"mode": {"rw"}, "_pragma": {"busy_timeout(5000)", "foreign_keys(1)"}}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	defer func() {
		if err != nil {
			db.Close()
		}
	}()
	if err := db.PingContext(ctx); err != nil {
		return nil, err
	}
	var gen [16]byte
	if create {
		for gen == [16]byte{} {
			if _, err := rand.Read(gen[:]); err != nil {
				return nil, err
			}
		}
		if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
			return nil, err
		}
		if _, err := db.ExecContext(ctx, `CREATE TABLE _dolmen_lakehouse_meta (singleton INTEGER PRIMARY KEY CHECK(singleton = 1), format INTEGER NOT NULL, namespace TEXT NOT NULL, generation BLOB NOT NULL CHECK(length(generation) = 16))`); err != nil {
			return nil, err
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO _dolmen_lakehouse_meta VALUES (1, ?, ?, ?)`, catalogFormat, name, gen[:]); err != nil {
			return nil, err
		}
	} else {
		var format int
		var stored string
		var raw []byte
		if err := db.QueryRowContext(ctx, `SELECT format, namespace, generation FROM _dolmen_lakehouse_meta WHERE singleton = 1`).Scan(&format, &stored, &raw); err != nil {
			return nil, fmt.Errorf("%w: lakehouse namespace %s has unreadable catalog metadata: %v", store.ErrCatalogCorrupt, name, err)
		}
		if format > catalogFormat {
			return nil, fmt.Errorf("%w: lakehouse namespace %s requires catalog format %d; upgrade dolmen", store.ErrCatalogTooNew, name, format)
		}
		if format < 1 || stored != name || len(raw) != 16 {
			return nil, fmt.Errorf("%w: lakehouse namespace %s has invalid catalog metadata", store.ErrCatalogCorrupt, name)
		}
		copy(gen[:], raw)
		if gen == [16]byte{} {
			return nil, fmt.Errorf("%w: lakehouse namespace %s has an empty generation", store.ErrCatalogCorrupt, name)
		}
	}
	if _, err := db.ExecContext(ctx, `PRAGMA synchronous=FULL`); err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS _dolmen_lakehouse_tables(name TEXT PRIMARY KEY, generation INTEGER NOT NULL CHECK(generation>=0))`); err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `UPDATE _dolmen_lakehouse_meta SET format=? WHERE singleton=1`, catalogFormat); err != nil {
		return nil, err
	}
	dataDir := filepath.Join(s.dir, namespacePath(name), "data")
	cat, err := sqlcatalog.NewCatalog("dolmen", db, sqlcatalog.SQLite, iceberg.Properties{"init_catalog_tables": "false", "warehouse": fileLocation(dataDir), "format-version": "2"})
	if err != nil {
		return nil, err
	}
	ident := table.Identifier(strings.Split(name, "/"))
	if create {
		if err := cat.CreateSQLTables(ctx); err != nil {
			return nil, err
		}
		if err := cat.CreateNamespace(ctx, ident, iceberg.Properties{"location": fileLocation(dataDir)}); err != nil {
			return nil, err
		}
	} else {
		exists, err := cat.CheckNamespaceExists(ctx, ident)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf("%w: lakehouse namespace %s is absent from its Iceberg catalog", store.ErrCatalogCorrupt, name)
		}
	}
	return &namespace{db: db, catalog: cat, generation: gen, dataDir: dataDir}, nil
}

func (s *Store) openNamespace(ctx context.Context, name string) (*namespace, error) {
	if err := s.checkNamespaceFiles(name); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: namespace %s does not exist; create_namespace first", store.ErrNotFound, name)
		}
		return nil, err
	}
	if n := s.namespaces[name]; n != nil {
		s.tick++
		n.lastUse = s.tick
		return n, nil
	}
	if err := s.makeRoom(); err != nil {
		return nil, err
	}
	n, err := s.openCatalog(ctx, name, namespacePath(name), false)
	if err != nil {
		return nil, err
	}
	s.tick++
	n.lastUse = s.tick
	s.namespaces[name] = n
	return n, nil
}
