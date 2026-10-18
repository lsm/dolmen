package lakehouse

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/gob"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	icebergio "github.com/apache/iceberg-go/io"
	"github.com/apache/iceberg-go/table"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

const vacuumStamp = "vacuum.stamp"

func (s *Store) stampVacuum(ns string, done bool) error {
	path := filepath.Join(s.dir, namespacePath(ns), vacuumStamp)
	n, _ := readStamp(path)
	switch {
	case !done && n%2 == 0:
		n++
	case !done:
		n += 2
	case n%2 == 1:
		n++
	}
	tmp := filepath.Join(filepath.Dir(path), "."+vacuumStamp+".tmp")
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(n, 10)), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readStamp(path string) (int64, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
}

type backupFS struct {
	localFS
	live, out string
}

func (f backupFS) Open(name string) (icebergio.File, error) {
	rel, err := filepath.Rel(f.live, localPath(name))
	if err != nil || !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("%s lies outside the data directory %s", name, f.live)
	}
	return f.localFS.LocalFS.Open(filepath.Join(f.out, rel))
}

func unreachable(live, outDir string, locations []string) (string, error) {
	fsys := backupFS{live: live, out: outDir}
	check := func(name string) (bool, error) {
		file, err := fsys.Open(name)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return true, file.Close()
	}
	for _, loc := range locations {
		file, err := fsys.Open(loc)
		if errors.Is(err, fs.ErrNotExist) {
			return loc, nil
		}
		if err != nil {
			return "", err
		}
		meta, err := table.ParseMetadata(file)
		file.Close()
		if err != nil {
			return "", err
		}
		for _, snap := range meta.Snapshots() {
			if ok, err := check(snap.ManifestList); err != nil || !ok {
				return snap.ManifestList, err
			}
			manifests, err := snap.Manifests(fsys)
			if err != nil {
				return "", err
			}
			for _, mf := range manifests {
				if ok, err := check(mf.FilePath()); err != nil || !ok {
					return mf.FilePath(), err
				}
				entries, err := mf.FetchEntries(fsys, true)
				if err != nil {
					return "", err
				}
				for _, e := range entries {
					if ok, err := check(e.DataFile().FilePath()); err != nil || !ok {
						return e.DataFile().FilePath(), err
					}
				}
			}
		}
	}
	return "", nil
}

func backupReferences(ctx context.Context, live, outDir, snapshot string, locations []string) (map[string]bool, error) {
	fsys := backupFS{live: live, out: outDir}
	refs := map[string]bool{}
	add := func(v any) error {
		if v == nil {
			return nil
		}
		ref, err := store.ParseFileReference(v)
		if err != nil {
			return err
		}
		refs[ref.SHA256] = true
		return nil
	}
	fields := map[string][]string{}
	for _, loc := range locations {
		file, err := fsys.Open(loc)
		if err != nil {
			return nil, err
		}
		meta, err := table.ParseMetadata(file)
		file.Close()
		if err != nil {
			return nil, err
		}
		var sc *schema.TableSchema
		if err := decodeProperty(meta.Properties()[schemaProperty], &sc); err != nil || sc == nil {
			return nil, fmt.Errorf("%w: invalid lakehouse table registry at %s", store.ErrCatalogCorrupt, loc)
		}
		var names []string
		for _, f := range sc.FileFields() {
			names = append(names, f.Name)
		}
		if len(names) == 0 {
			continue
		}
		fields[sc.Name] = names
		if meta.CurrentSnapshot() == nil {
			continue
		}
		tbl := table.New(tableIdentifier(sc.Namespace, sc.Name), meta, loc, func(context.Context) (icebergio.IO, error) { return fsys, nil }, nil)
		_, records, err := tbl.Scan(table.WithSelectedFields(names...)).ToArrowRecords(ctx)
		if err != nil {
			return nil, err
		}
		for rec, err := range records {
			if err != nil {
				return nil, err
			}
			for row := 0; row < int(rec.NumRows()) && err == nil; row++ {
				for col := 0; col < int(rec.NumCols()) && err == nil; col++ {
					err = add(cellValue(rec.Column(col), row))
				}
			}
			rec.Release()
			if err != nil {
				return nil, err
			}
		}
	}
	if len(fields) == 0 {
		return refs, nil
	}
	db, err := sql.Open("sqlite", readOnlyDSN(snapshot))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT table_name, rows FROM _dolmen_lakehouse_commits WHERE materialized = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var payload []byte
		if err := rows.Scan(&name, &payload); err != nil {
			return nil, err
		}
		names := fields[name]
		if len(names) == 0 {
			continue
		}
		var decoded commitRows
		if err := gob.NewDecoder(bytes.NewReader(payload)).Decode(&decoded); err != nil {
			return nil, fmt.Errorf("%w: unreadable lakehouse commit payload: %v", store.ErrCatalogCorrupt, err)
		}
		for _, r := range decoded.Rows {
			for _, f := range names {
				if err := add(r[f]); err != nil {
					return nil, err
				}
			}
		}
	}
	return refs, rows.Err()
}
