package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackupRefusesADataDirectoryThatStoresFiles(t *testing.T) {
	for _, ns := range []string{"app", "team/app"} {
		dir := t.TempDir()
		st, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.CreateNamespace(context.Background(), ns, [16]byte{}); err != nil {
			t.Fatal(err)
		}
		st.Close()
		blob := filepath.Join(dir, filepath.FromSlash(ns)+".files", "ab", "abcd")
		if err := os.MkdirAll(filepath.Dir(blob), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(blob, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err = Backup(context.Background(), dir, filepath.Join(t.TempDir(), "out"), nil)
		if err == nil || !strings.Contains(err.Error(), "file fields") {
			t.Fatalf("namespace %s: backup of a data directory holding files must be refused, got %v", ns, err)
		}
	}
}
