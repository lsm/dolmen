//go:build windows

package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestABackupReadOpensALongWindowsPath(t *testing.T) {
	dir := t.TempDir()
	for len(dir) < 300 {
		dir = filepath.Join(dir, strings.Repeat("d", 40))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "data.parquet")
	if err := os.WriteFile(path, []byte("rows"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openShared(path)
	if err != nil {
		t.Fatalf("open a %d-character path: %v", len(path), err)
	}
	f.Close()
}
