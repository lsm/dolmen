package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	BackupManifestFile   = "manifest.json"
	backupManifestFormat = 1
	restoringSuffix      = ".restoring"
)

type BackupManifest struct {
	Format    int          `json:"format"`
	CreatedAt string       `json:"created_at"`
	Files     []BackupFile `json:"files"`
}

type BackupFile struct {
	Namespace     string `json:"namespace,omitempty"`
	Path          string `json:"path"`
	Bytes         int64  `json:"bytes"`
	SHA256        string `json:"sha256"`
	CatalogFormat int    `json:"catalog_format,omitempty"`
	MinReader     int    `json:"catalog_min_reader,omitempty"`
}

func nsFile(dir, name string) string {
	segs := strings.Split(name, "/")
	segs[len(segs)-1] += ".db"
	return filepath.Join(append([]string{dir}, segs...)...)
}

func Backup(ctx context.Context, dataDir, outDir string, extraFiles []string) (BackupManifest, error) {
	if err := emptyOrAbsent(outDir); err != nil {
		return BackupManifest{}, err
	}
	var names []string
	if err := walkNamespaces(dataDir, "", &names); err != nil {
		return BackupManifest{}, fmt.Errorf("list namespaces in %s: %w", dataDir, err)
	}
	sortNS(names)
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return BackupManifest{}, err
	}
	m := BackupManifest{Format: backupManifestFormat, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	for _, name := range names {
		rel, err := filepath.Rel(dataDir, nsFile(dataDir, name))
		if err != nil {
			return BackupManifest{}, err
		}
		f, err := snapshotFile(ctx, filepath.Join(dataDir, rel), outDir, rel)
		if err != nil {
			return BackupManifest{}, fmt.Errorf("back up namespace %s: %w", name, err)
		}
		f.Namespace = name
		m.Files = append(m.Files, f)
	}
	for _, rel := range extraFiles {
		src := filepath.Join(dataDir, rel)
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			continue
		}
		f, err := snapshotFile(ctx, src, outDir, rel)
		if err != nil {
			return BackupManifest{}, fmt.Errorf("back up %s: %w", rel, err)
		}
		m.Files = append(m.Files, f)
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return BackupManifest{}, err
	}
	if err := writeFileSynced(filepath.Join(outDir, BackupManifestFile), append(raw, '\n')); err != nil {
		return BackupManifest{}, err
	}
	return m, nil
}

func emptyOrAbsent(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("backup directory %s is not empty; give each backup a directory of its own", dir)
	}
	return nil
}

func snapshotFile(ctx context.Context, src, outDir, rel string) (BackupFile, error) {
	dst := filepath.Join(outDir, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return BackupFile{}, err
	}
	db, err := sql.Open("sqlite", dsn(src, true))
	if err != nil {
		return BackupFile{}, err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", dst); err != nil {
		return BackupFile{}, err
	}
	if err := os.Chmod(dst, 0o600); err != nil {
		return BackupFile{}, err
	}
	f := BackupFile{Path: filepath.ToSlash(rel)}
	if f.Bytes, f.SHA256, err = fileDigest(dst); err != nil {
		return BackupFile{}, err
	}
	if f.CatalogFormat, f.MinReader, err = catalogOf(ctx, dst); err != nil {
		return BackupFile{}, err
	}
	return f, nil
}

func catalogOf(ctx context.Context, path string) (int, int, error) {
	db, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()
	var hasMeta int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = '_dolmen_meta'`).Scan(&hasMeta); err != nil {
		return 0, 0, err
	}
	if hasMeta == 0 {
		return 0, 0, nil
	}
	return readCatalogVersion(ctx, db)
}

func fileDigest(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

func writeFileSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func ReadBackupManifest(dir string) (BackupManifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, BackupManifestFile))
	if errors.Is(err, os.ErrNotExist) {
		return BackupManifest{}, fmt.Errorf("%s has no %s, so it is not a finished backup; an interrupted backup writes its manifest last and never leaves one behind", dir, BackupManifestFile)
	}
	if err != nil {
		return BackupManifest{}, err
	}
	var m BackupManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return BackupManifest{}, fmt.Errorf("%s in %s is not a backup manifest: %w", BackupManifestFile, dir, err)
	}
	if m.Format != backupManifestFormat {
		return BackupManifest{}, fmt.Errorf("backup manifest format %d is not one this binary reads (it reads %d)", m.Format, backupManifestFormat)
	}
	return m, nil
}

func Restore(ctx context.Context, fromDir, dataDir string) (BackupManifest, error) {
	m, err := ReadBackupManifest(fromDir)
	if err != nil {
		return BackupManifest{}, err
	}
	var pending []BackupFile
	for _, f := range m.Files {
		rel := filepath.FromSlash(f.Path)
		if !filepath.IsLocal(rel) {
			return BackupManifest{}, fmt.Errorf("manifest names %q, which is not a path inside the backup", f.Path)
		}
		if err := verifyBackupFile(ctx, filepath.Join(fromDir, rel), f); err != nil {
			return BackupManifest{}, err
		}
		target := filepath.Join(dataDir, rel)
		restored, err := alreadyRestored(target, f)
		if err != nil {
			return BackupManifest{}, err
		}
		if !restored {
			pending = append(pending, f)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].Path < pending[j].Path })
	for _, f := range pending {
		rel := filepath.FromSlash(f.Path)
		if err := activate(filepath.Join(fromDir, rel), filepath.Join(dataDir, rel)); err != nil {
			return BackupManifest{}, fmt.Errorf("restore %s: %w", f.Path, err)
		}
	}
	return m, nil
}

func verifyBackupFile(ctx context.Context, path string, f BackupFile) error {
	n, sum, err := fileDigest(path)
	if err != nil {
		return fmt.Errorf("backup file %s: %w", f.Path, err)
	}
	if n != f.Bytes || sum != f.SHA256 {
		return fmt.Errorf("backup file %s does not match its manifest (%d bytes, sha256 %s; the manifest says %d bytes, %s); the backup is damaged, so nothing was restored", f.Path, n, sum, f.Bytes, f.SHA256)
	}
	db, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		return err
	}
	defer db.Close()
	var check string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&check); err != nil {
		return fmt.Errorf("backup file %s fails its integrity check: %w", f.Path, err)
	}
	if check != "ok" {
		return fmt.Errorf("backup file %s fails its integrity check (%s); nothing was restored", f.Path, check)
	}
	if f.Namespace != "" && f.MinReader > CatalogFormat {
		return &CatalogVersionError{Namespace: f.Namespace, Format: f.CatalogFormat, MinReader: f.MinReader, Supported: CatalogFormat}
	}
	return nil
}

func alreadyRestored(target string, f BackupFile) (bool, error) {
	for _, side := range []string{target + "-wal", target + "-shm"} {
		if _, err := os.Stat(side); err == nil {
			return false, fmt.Errorf("%s is in use (its %s file exists); restore into a data directory no server is using", target, filepath.Ext(side))
		}
	}
	n, sum, err := fileDigest(target)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if n == f.Bytes && sum == f.SHA256 {
		return true, nil
	}
	return false, fmt.Errorf("%s already exists and differs from the backup; restore never overwrites data, so drop it or restore into another data directory", target)
}

func activate(src, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	tmp := target + restoringSuffix
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	for _, side := range []string{target + "-wal", target + "-shm"} {
		if _, err := os.Stat(side); err == nil {
			os.Remove(tmp)
			return fmt.Errorf("%s came into use during the restore (its %s file appeared); nothing was overwritten, so stop the server and run the restore again", target, filepath.Ext(side))
		}
	}
	if err := os.Link(tmp, target); err != nil {
		os.Remove(tmp)
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s appeared during the restore; restore never overwrites data, so stop the server and run the restore again", target)
		}
		return err
	}
	return os.Remove(tmp)
}
