package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestABackupReadDoesNotBlockDeletingItsSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.parquet")
	if err := os.WriteFile(path, []byte("rows"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openShared(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove while a backup reads the file: %v", err)
	}
}
