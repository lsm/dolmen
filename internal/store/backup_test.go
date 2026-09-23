package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func countNotes(t *testing.T, dataDir, ns string) int {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(nsFile(dataDir, ns), true))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM notes`).Scan(&n); err != nil {
		t.Fatalf("count notes in %s: %v", ns, err)
	}
	return n
}

func seededBackup(t *testing.T) (dataDir, outDir string) {
	t.Helper()
	dataDir = t.TempDir()
	st := openStoreAt(t, dataDir)
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)
	outDir = filepath.Join(t.TempDir(), "backup")
	if _, err := Backup(context.Background(), dataDir, outDir, nil); err != nil {
		t.Fatalf("backup: %v", err)
	}
	return dataDir, outDir
}

func TestABackupIsConsistentWhileWritesContinue(t *testing.T) {
	dataDir := t.TempDir()
	st := openStoreAt(t, dataDir)
	mustCreateNotes(t, st)
	var written atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := st.Insert(context.Background(), "test", "notes", []map[string]any{{"title": "during", "body": "backup"}}, testEmbed); err == nil {
				written.Add(1)
			}
		}
	}()
	for written.Load() < 20 {
	}
	outDir := filepath.Join(t.TempDir(), "backup")
	m, err := Backup(context.Background(), dataDir, outDir, nil)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("a backup must succeed while writes continue: %v", err)
	}
	if len(m.Files) != 1 || m.Files[0].Namespace != "test" || m.Files[0].SHA256 == "" || m.Files[0].CatalogFormat != CatalogFormat {
		t.Fatalf("manifest: %+v", m)
	}

	restored := t.TempDir()
	if _, err := Restore(context.Background(), outDir, restored); err != nil {
		t.Fatalf("restore: %v", err)
	}
	n := countNotes(t, restored, "test")
	if n < 20 || int64(n) > written.Load() {
		t.Fatalf("the restored namespace holds %d rows; a consistent snapshot holds at least the 20 written before the backup began and no more than the %d written in all", n, written.Load())
	}
	re := openStoreAt(t, restored)
	if _, _, err := re.Query(context.Background(), "test", "SELECT count(*) AS n FROM notes", nil, 0, 10); err != nil {
		t.Fatalf("the restored namespace must open and answer through the store: %v", err)
	}
}

func TestARestoreRefusesADamagedBackup(t *testing.T) {
	_, outDir := seededBackup(t)
	path := filepath.Join(outDir, "test.db")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 0xff
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	_, err = Restore(context.Background(), outDir, target)
	if err == nil || !strings.Contains(err.Error(), "does not match its manifest") {
		t.Fatalf("got %v, want the damage named", err)
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Fatalf("a refused restore must write nothing, found %d entries", len(entries))
	}
}

func TestARestoreRefusesAnUnfinishedBackup(t *testing.T) {
	_, outDir := seededBackup(t)
	if err := os.Remove(filepath.Join(outDir, BackupManifestFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), outDir, t.TempDir()); err == nil || !strings.Contains(err.Error(), "not a finished backup") {
		t.Fatalf("got %v", err)
	}
}

func TestARestoreNeverOverwritesData(t *testing.T) {
	dataDir, outDir := seededBackup(t)
	st := openStoreAt(t, dataDir)
	if _, err := st.Insert(context.Background(), "test", "notes", []map[string]any{{"title": "after"}}, testEmbed); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), outDir, dataDir); err == nil || !strings.Contains(err.Error(), "in use") && !strings.Contains(err.Error(), "differs from the backup") {
		t.Fatalf("restoring over a live namespace must be refused, got %v", err)
	}
}

func TestARestoreResumesAfterAnInterruption(t *testing.T) {
	_, outDir := seededBackup(t)
	target := t.TempDir()
	if _, err := Restore(context.Background(), outDir, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(target, "test.db")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "test.db"+restoringSuffix), []byte("half a copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), outDir, target); err != nil {
		t.Fatalf("a restore interrupted mid-copy must complete when run again: %v", err)
	}
	if _, err := Restore(context.Background(), outDir, target); err != nil {
		t.Fatalf("running a finished restore again must find everything already in place: %v", err)
	}
	if n := countNotes(t, target, "test"); n != 3 {
		t.Fatalf("restored %d rows, want 3", n)
	}
}

func TestARestoreRefusesACatalogThisBinaryCannotRead(t *testing.T) {
	dataDir := t.TempDir()
	seedNamespace(t, dataDir, "future")
	setCatalogMeta(t, dataDir, "future", catalogMinReaderKey, "99")
	outDir := filepath.Join(t.TempDir(), "backup")
	if _, err := Backup(context.Background(), dataDir, outDir, nil); err != nil {
		t.Fatal(err)
	}
	var cve *CatalogVersionError
	if _, err := Restore(context.Background(), outDir, t.TempDir()); !errors.As(err, &cve) {
		t.Fatalf("got %v, want a CatalogVersionError", err)
	}
}

func TestABackupNeedsADirectoryOfItsOwn(t *testing.T) {
	dataDir, outDir := seededBackup(t)
	if _, err := Backup(context.Background(), dataDir, outDir, nil); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("a second backup into the same directory must be refused, got %v", err)
	}
}

func TestABackupCarriesNamedExtraFiles(t *testing.T) {
	dataDir := t.TempDir()
	st := openStoreAt(t, dataDir)
	mustCreateNotes(t, st)
	extra, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dataDir, "_extra.db"))+"?mode=rwc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := extra.Exec(`CREATE TABLE grants (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	extra.Close()
	outDir := filepath.Join(t.TempDir(), "backup")
	m, err := Backup(context.Background(), dataDir, outDir, []string{"_extra.db", "_absent.db"})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 2 || m.Files[1].Path != "_extra.db" || m.Files[1].Namespace != "" {
		t.Fatalf("the extra file must be carried and the absent one skipped: %+v", m.Files)
	}
	target := t.TempDir()
	if _, err := Restore(context.Background(), outDir, target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "_extra.db")); err != nil {
		t.Fatalf("the extra file must be restored: %v", err)
	}
}

func TestABackupKeepsNestedNamespacesInPlace(t *testing.T) {
	dataDir := t.TempDir()
	seedNamespace(t, dataDir, "acme")
	seedNamespace(t, dataDir, "acme/team-a")
	outDir := filepath.Join(t.TempDir(), "backup")
	m, err := Backup(context.Background(), dataDir, outDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 2 || m.Files[1].Namespace != "acme/team-a" || m.Files[1].Path != "acme/team-a.db" {
		t.Fatalf("manifest: %+v", m.Files)
	}
	target := t.TempDir()
	if _, err := Restore(context.Background(), outDir, target); err != nil {
		t.Fatal(err)
	}
	st := openStoreAt(t, target)
	names, err := st.ListNamespaces()
	if err != nil || strings.Join(names, ",") != "acme,acme/team-a" {
		t.Fatalf("restored namespaces %v %v", names, err)
	}
}
