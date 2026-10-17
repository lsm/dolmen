package lakehouse

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lsm/dolmen/internal/store"
)

var backupAttempts, backupPause = 5, 200 * time.Millisecond

var errTornStamp = errors.New("its vacuum stamp was being written as it was read; rerun the backup")

func Backup(ctx context.Context, dataDir, outDir string, extraFiles []string) (store.BackupManifest, error) {
	live, err := filepath.Abs(dataDir)
	if err == nil {
		live, err = filepath.EvalSymlinks(live)
	}
	if err != nil {
		return store.BackupManifest{}, err
	}
	names, err := backupNamespaces(live)
	if err != nil {
		return store.BackupManifest{}, fmt.Errorf("list namespaces in %s: %w", dataDir, err)
	}
	if err := store.PrepareBackupDir(outDir); err != nil {
		return store.BackupManifest{}, err
	}
	m := store.BackupManifest{Engine: store.EngineLakehouse, DataDir: live}
	for _, ns := range names {
		var err error
		for attempt := 1; ; attempt++ {
			var retry bool
			retry, err = backupNamespace(ctx, live, outDir, ns, &m)
			if !retry || attempt == backupAttempts {
				break
			}
			select {
			case <-ctx.Done():
				return store.BackupManifest{}, ctx.Err()
			case <-time.After(backupPause):
			}
		}
		if err != nil {
			return store.BackupManifest{}, fmt.Errorf("back up namespace %s: %w", ns, err)
		}
	}
	for _, rel := range extraFiles {
		src := filepath.Join(live, rel)
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			continue
		}
		f, err := store.SnapshotSQLite(ctx, src, outDir, rel)
		if err != nil {
			return store.BackupManifest{}, fmt.Errorf("back up %s: %w", rel, err)
		}
		m.Files = append(m.Files, f)
	}
	if err := store.WriteBackupManifest(outDir, &m); err != nil {
		return store.BackupManifest{}, err
	}
	return m, nil
}

func backupNamespaces(live string) ([]string, error) {
	var names []string
	err := filepath.WalkDir(live, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() || path == live {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			return fs.SkipDir
		}
		stem, ok := strings.CutSuffix(d.Name(), ".lakehouse")
		if !ok {
			return nil
		}
		if _, err := os.Stat(filepath.Join(path, "catalog.db")); err == nil {
			rel, err := filepath.Rel(live, filepath.Join(filepath.Dir(path), stem))
			if err != nil {
				return err
			}
			names = append(names, filepath.ToSlash(rel))
		}
		return fs.SkipDir
	})
	sort.Strings(names)
	return names, err
}

func backupNamespace(ctx context.Context, live, outDir, ns string, m *store.BackupManifest) (bool, error) {
	rel := namespacePath(ns)
	stampPath := filepath.Join(live, rel, vacuumStamp)
	before, err := readStamp(stampPath)
	if errors.Is(err, strconv.ErrSyntax) {
		return true, errTornStamp
	}
	if err != nil {
		return false, err
	}
	if before%2 == 1 {
		return true, fmt.Errorf("a vacuum of it is running, or was interrupted; wait for it to finish, or run vacuum on the namespace again, then rerun the backup")
	}
	for _, dir := range []string{rel, filepath.FromSlash(ns) + ".files"} {
		if err := os.RemoveAll(filepath.Join(outDir, dir)); err != nil {
			return false, err
		}
	}
	catalog, err := store.SnapshotSQLite(ctx, filepath.Join(live, rel, "catalog.db"), outDir, filepath.Join(rel, "catalog.db"))
	if err != nil {
		return false, err
	}
	catalog.Namespace = ns
	snapshot := filepath.Join(outDir, rel, "catalog.db")
	locations, inBatch, err := snapshotTables(ctx, snapshot)
	if err != nil {
		return false, err
	}
	if inBatch {
		return true, fmt.Errorf("a batch was in progress each time it was read; rerun the backup")
	}
	data, err := copyTree(live, outDir, filepath.Join(rel, "data"))
	if err != nil {
		return false, err
	}
	blobs, err := copyTree(live, outDir, filepath.FromSlash(ns)+".files")
	if err != nil {
		return false, err
	}
	if missing, err := unreachable(live, outDir, locations); err != nil {
		return false, err
	} else if missing != "" {
		return true, fmt.Errorf("file %s, which its catalog references, changed while it was copied; rerun the backup", missing)
	}
	if after, err := readStamp(stampPath); errors.Is(err, strconv.ErrSyntax) {
		return true, errTornStamp
	} else if err != nil {
		return false, err
	} else if after != before {
		return true, fmt.Errorf("a vacuum ran on it while it was copied; rerun the backup")
	}
	for i := range data {
		data[i].Namespace = ns
	}
	for i := range blobs {
		blobs[i].Namespace = ns
	}
	m.Files = append(m.Files, catalog)
	m.DataFiles = append(m.DataFiles, data...)
	m.Blobs = append(m.Blobs, blobs...)
	return false, nil
}

func snapshotTables(ctx context.Context, path string) ([]string, bool, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, false, err
	}
	defer db.Close()
	var intents int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM _dolmen_lakehouse_batch_intent`).Scan(&intents); err != nil {
		return nil, false, err
	}
	rows, err := db.QueryContext(ctx, `SELECT metadata_location FROM iceberg_tables WHERE metadata_location IS NOT NULL ORDER BY metadata_location`)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var loc string
		if err := rows.Scan(&loc); err != nil {
			return nil, false, err
		}
		out = append(out, loc)
	}
	return out, intents > 0, rows.Err()
}

func copyTree(live, outDir, rel string) ([]store.BackupBlob, error) {
	var out []store.BackupBlob
	err := filepath.WalkDir(filepath.Join(live, rel), func(path string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		r, err := filepath.Rel(live, path)
		if err != nil {
			return err
		}
		b, err := store.CopyIntoBackup(path, outDir, r)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		out = append(out, b)
		return nil
	})
	return out, err
}
