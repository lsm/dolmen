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
	blobManifestFormat   = 2
	engineManifestFormat = 3
	restoringSuffix      = ".restoring"
)

type BackupManifest struct {
	Format    int          `json:"format"`
	CreatedAt string       `json:"created_at"`
	Engine    string       `json:"engine,omitempty"`
	DataDir   string       `json:"data_dir,omitempty"`
	Files     []BackupFile `json:"files"`
	Dirs      []string     `json:"dirs,omitempty"`
	DataFiles []BackupBlob `json:"data_files,omitempty"`
	Blobs     []BackupBlob `json:"blobs,omitempty"`
}

type BackupBlob struct {
	Namespace string `json:"namespace"`
	Path      string `json:"path"`
	Bytes     int64  `json:"bytes"`
	SHA256    string `json:"sha256"`
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
	var names []string
	if err := walkNamespaces(dataDir, "", &names); err != nil {
		return BackupManifest{}, fmt.Errorf("list namespaces in %s: %w", dataDir, err)
	}
	sortNS(names)
	if err := PrepareBackupDir(outDir); err != nil {
		return BackupManifest{}, err
	}
	var m BackupManifest
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
		blobs, err := backupBlobs(ctx, dataDir, outDir, name, filepath.Join(outDir, rel))
		if err != nil {
			return BackupManifest{}, err
		}
		m.Blobs = append(m.Blobs, blobs...)
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
	if err := WriteBackupManifest(outDir, &m); err != nil {
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
	if err := secureDir(filepath.Dir(dst)); err != nil {
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
	if m.Format < backupManifestFormat || m.Format > engineManifestFormat {
		return BackupManifest{}, fmt.Errorf("backup manifest format %d is not one this binary reads (it reads %d to %d)", m.Format, backupManifestFormat, engineManifestFormat)
	}
	return m, nil
}

func Restore(ctx context.Context, fromDir, dataDir string) (BackupManifest, error) {
	m, err := ReadBackupManifest(fromDir)
	if err != nil {
		return BackupManifest{}, err
	}
	if m.DataDir != "" && !sameDir(dataDir, m.DataDir) {
		return BackupManifest{}, fmt.Errorf("this %s backup was taken from data directory %s and its catalogs record absolute paths, so it restores only there; restore with -data %s", m.Engine, m.DataDir, m.DataDir)
	}
	var data []BackupBlob
	for _, b := range m.DataFiles {
		rel := filepath.FromSlash(b.Path)
		if !filepath.IsLocal(rel) {
			return BackupManifest{}, fmt.Errorf("manifest names data file %q, which is not a path inside the backup", b.Path)
		}
		if err := verifyBlob(filepath.Join(fromDir, rel), b); err != nil {
			return BackupManifest{}, err
		}
		restored, err := blobRestored(filepath.Join(dataDir, rel), b, m.DataDir)
		if err != nil {
			return BackupManifest{}, err
		}
		if !restored {
			data = append(data, b)
		}
	}
	var blobs []BackupBlob
	for _, b := range m.Blobs {
		rel := filepath.FromSlash(b.Path)
		if !filepath.IsLocal(rel) || !strings.HasSuffix(b.Path, "/"+b.SHA256) {
			return BackupManifest{}, fmt.Errorf("manifest names file %q, which is not a file store path inside the backup", b.Path)
		}
		if err := verifyBlob(filepath.Join(fromDir, rel), b); err != nil {
			return BackupManifest{}, err
		}
		restored, err := blobRestored(filepath.Join(dataDir, rel), b, m.DataDir)
		if err != nil {
			return BackupManifest{}, err
		}
		if !restored {
			blobs = append(blobs, b)
		}
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
		restored, err := alreadyRestored(target, f, m.DataDir)
		if err != nil {
			return BackupManifest{}, err
		}
		if !restored {
			pending = append(pending, f)
		}
	}
	for _, d := range m.Dirs {
		rel := filepath.FromSlash(d)
		if !filepath.IsLocal(rel) {
			return BackupManifest{}, fmt.Errorf("manifest names directory %q, which is not a path inside the data directory", d)
		}
		if err := os.MkdirAll(filepath.Join(dataDir, rel), 0o700); err != nil {
			return BackupManifest{}, fmt.Errorf("restore directory %s: %w", d, err)
		}
	}
	for _, b := range append(blobs, data...) {
		rel := filepath.FromSlash(b.Path)
		if err := activate(filepath.Join(fromDir, rel), filepath.Join(dataDir, rel)); err != nil {
			return BackupManifest{}, fmt.Errorf("restore %s: %w", b.Path, err)
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

func alreadyRestored(target string, f BackupFile, onlyInto string) (bool, error) {
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
	if onlyInto != "" {
		return false, fmt.Errorf("%s already exists and differs from the backup; restore never overwrites data, and this backup restores only into %s, so drop that namespace there and restore again", target, onlyInto)
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

func backupBlobs(ctx context.Context, dataDir, outDir, ns, snapshot string) ([]BackupBlob, error) {
	db, err := sql.Open("sqlite", dsn(snapshot, true))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	refs, err := fileReferences(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("back up the files of namespace %s: %w", ns, err)
	}
	sums := make([]string, 0, len(refs))
	for sum := range refs {
		sums = append(sums, sum)
	}
	sort.Strings(sums)
	var out []BackupBlob
	for _, sum := range sums {
		rel := filepath.Join(filepath.FromSlash(ns)+".files", sum[:2], sum)
		b := BackupBlob{Namespace: ns, Path: filepath.ToSlash(rel), SHA256: sum}
		n, got, err := copyFileSynced(filepath.Join(dataDir, rel), filepath.Join(outDir, rel))
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("namespace %s references file %s, which is missing from the data directory; if a vacuum removed it after its row changed, run the backup again", ns, sum)
		}
		if err != nil {
			return nil, fmt.Errorf("back up file %s of namespace %s: %w", sum, ns, err)
		}
		if got != sum {
			return nil, fmt.Errorf("file %s of namespace %s does not hash to its name (sha256 %s); the file store is damaged", sum, ns, got)
		}
		b.Bytes = n
		out = append(out, b)
	}
	return out, nil
}

func copyFileSynced(src, dst string) (int64, string, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, "", err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return 0, "", err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return 0, "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

func verifyBlob(path string, b BackupBlob) error {
	n, sum, err := fileDigest(path)
	if err != nil {
		return fmt.Errorf("backup file %s: %w", b.Path, err)
	}
	if n != b.Bytes || sum != b.SHA256 {
		return fmt.Errorf("backup file %s does not match its manifest (%d bytes, sha256 %s; the manifest says %d bytes, %s); the backup is damaged, so nothing was restored", b.Path, n, sum, b.Bytes, b.SHA256)
	}
	return nil
}

func blobRestored(target string, b BackupBlob, onlyInto string) (bool, error) {
	n, sum, err := fileDigest(target)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if n == b.Bytes && sum == b.SHA256 {
		return true, nil
	}
	if onlyInto != "" {
		return false, fmt.Errorf("%s already exists and differs from the backup; restore never overwrites data, and this backup restores only into %s, so remove that file there and restore again", target, onlyInto)
	}
	return false, fmt.Errorf("%s already exists and differs from the backup; restore never overwrites data, so remove it or restore into an empty data directory", target)
}

func sameDir(a, b string) bool {
	resolve := func(p string) string {
		abs, err := filepath.Abs(p)
		if err != nil {
			return p
		}
		if r, err := filepath.EvalSymlinks(abs); err == nil {
			return r
		}
		return abs
	}
	return filepath.Clean(a) == filepath.Clean(b) || resolve(a) == resolve(b)
}

func PrepareBackupDir(outDir string) error {
	if err := emptyOrAbsent(outDir); err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return err
	}
	return secureDir(outDir)
}

func SnapshotSQLite(ctx context.Context, src, outDir, rel string) (BackupFile, error) {
	return snapshotFile(ctx, src, outDir, rel)
}

func CopyIntoBackup(src, outDir, rel string) (BackupBlob, error) {
	n, sum, err := copyFileSynced(src, filepath.Join(outDir, rel))
	if err != nil {
		return BackupBlob{}, err
	}
	return BackupBlob{Path: filepath.ToSlash(rel), Bytes: n, SHA256: sum}, nil
}

func WriteBackupManifest(outDir string, m *BackupManifest) error {
	m.Format = backupManifestFormat
	switch {
	case m.Engine != "":
		m.Format = engineManifestFormat
	case len(m.Blobs) > 0:
		m.Format = blobManifestFormat
	}
	if m.CreatedAt == "" {
		m.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeFileSynced(filepath.Join(outDir, BackupManifestFile), append(raw, '\n'))
}

func NamespacesIn(dataDir string) ([]string, error) {
	var names []string
	if err := walkNamespaces(dataDir, "", &names); err != nil {
		return nil, err
	}
	sortNS(names)
	return names, nil
}
