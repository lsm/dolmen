package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func engineBackup(t *testing.T, source string) string {
	t.Helper()
	live := t.TempDir()
	catalog := filepath.Join(live, "app.lakehouse", "catalog.db")
	if err := os.MkdirAll(filepath.Dir(catalog), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", catalog)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE t(x)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	data := filepath.Join(live, "app.lakehouse", "data", "t", "part.parquet")
	if err := os.MkdirAll(filepath.Dir(data), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(data, []byte("parquet bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "backup")
	if err := PrepareBackupDir(out); err != nil {
		t.Fatal(err)
	}
	m := BackupManifest{Engine: "lakehouse", DataDir: source}
	raw, err := CopyIntoBackup(data, out, filepath.Join("app.lakehouse", "data", "t", "part.parquet"))
	if err != nil {
		t.Fatal(err)
	}
	raw.Namespace = "app"
	m.DataFiles = append(m.DataFiles, raw)
	cat, err := SnapshotSQLite(context.Background(), catalog, out, filepath.Join("app.lakehouse", "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	cat.Namespace = "app"
	m.Files = append(m.Files, cat)
	if err := WriteBackupManifest(out, &m); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAnEngineBackupRestoresIntoItsOwnDataDirectory(t *testing.T) {
	target := t.TempDir()
	out := engineBackup(t, target)
	m, err := ReadBackupManifest(out)
	if err != nil || m.Engine != "lakehouse" || m.Format != engineManifestFormat {
		t.Fatalf("manifest = %+v, %v", m, err)
	}
	if _, err := Restore(context.Background(), out, target); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, "app.lakehouse", "data", "t", "part.parquet"))
	if err != nil || string(got) != "parquet bytes" {
		t.Fatalf("restored data file = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(target, "app.lakehouse", "catalog.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), out, target); err != nil {
		t.Fatalf("restoring again over identical files: %v", err)
	}
}

func TestAnEngineBackupRefusesAnotherDataDirectory(t *testing.T) {
	out := engineBackup(t, t.TempDir())
	other := t.TempDir()
	if _, err := Restore(context.Background(), out, other); err == nil || !strings.Contains(err.Error(), "absolute paths") {
		t.Fatalf("restore into another directory: %v", err)
	}
	if entries, _ := os.ReadDir(other); len(entries) != 0 {
		t.Fatalf("a refused restore wrote %d entries", len(entries))
	}
}

func TestAnEngineBackupRefusesADamagedDataFile(t *testing.T) {
	target := t.TempDir()
	out := engineBackup(t, target)
	if err := os.WriteFile(filepath.Join(out, "app.lakehouse", "data", "t", "part.parquet"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), out, target); err == nil || !strings.Contains(err.Error(), "does not match its manifest") {
		t.Fatalf("restore of a damaged data file: %v", err)
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Fatalf("a refused restore wrote %d entries", len(entries))
	}
}

func TestAnEngineRestoreOverDifferentDataSaysWhereItMustGo(t *testing.T) {
	target := t.TempDir()
	out := engineBackup(t, target)
	catalog := filepath.Join(target, "app.lakehouse", "catalog.db")
	if err := os.MkdirAll(filepath.Dir(catalog), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalog, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Restore(context.Background(), out, target)
	if err == nil || strings.Contains(err.Error(), "another data directory") || !strings.Contains(err.Error(), "only into "+target) {
		t.Fatalf("restore over a different catalog: %v", err)
	}
}

func TestAnEngineRestoreOverADifferentDataFileSaysWhereItMustGo(t *testing.T) {
	target := t.TempDir()
	out := engineBackup(t, target)
	data := filepath.Join(target, "app.lakehouse", "data", "t", "part.parquet")
	if err := os.MkdirAll(filepath.Dir(data), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(data, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), out, target); err == nil || !strings.Contains(err.Error(), "only into "+target) {
		t.Fatalf("restore over a different data file: %v", err)
	}
}
