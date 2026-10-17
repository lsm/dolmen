package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func storedBlob(t *testing.T, dir, ns, content string) FileBlob {
	t.Helper()
	h := sha256.Sum256([]byte(content))
	sum := hex.EncodeToString(h[:])
	path := filepath.Join(dir, filepath.FromSlash(ns)+".files", sum[:2], sum)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return FileBlob{SHA256: sum, Size: int64(len(content)), MediaType: "text/plain"}
}

func blobDataDir(t *testing.T) (string, []FileBlob, FileBlob) {
	t.Helper()
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateNamespace(ctx, "team/app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTable(ctx, "team/app", "docs", []schema.Field{{Name: "doc", Type: schema.File}}, TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	kept := []FileBlob{storedBlob(t, dir, "team/app", "first"), storedBlob(t, dir, "team/app", "second")}
	garbage := storedBlob(t, dir, "team/app", "unreferenced")
	src := &fakeFiles{parts: map[string]FileBlob{"a": kept[0], "b": kept[1]}}
	if _, err := st.Insert(WithFileSource(ctx, src), "team/app", "docs", []map[string]any{
		{"doc": map[string]any{"part": "a"}}, {"doc": map[string]any{"part": "b"}}, {"doc": map[string]any{"part": "a"}},
	}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	return dir, kept, garbage
}

func blobAt(dir string, b FileBlob) string {
	return filepath.Join(dir, "team", "app.files", b.SHA256[:2], b.SHA256)
}

func TestABackupCarriesTheFilesItsRowsReference(t *testing.T) {
	dir, kept, garbage := blobDataDir(t)
	out := filepath.Join(t.TempDir(), "backup")
	m, err := Backup(context.Background(), dir, out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Blobs) != 2 {
		t.Fatalf("manifest lists %d files, want the 2 referenced ones: %+v", len(m.Blobs), m.Blobs)
	}
	restored := t.TempDir()
	if _, err := Restore(context.Background(), out, restored); err != nil {
		t.Fatal(err)
	}
	for _, b := range kept {
		got, err := os.ReadFile(blobAt(restored, b))
		if err != nil || int64(len(got)) != b.Size {
			t.Fatalf("restored file %s: %q %v", b.SHA256, got, err)
		}
	}
	if _, err := os.Stat(blobAt(restored, garbage)); !os.IsNotExist(err) {
		t.Fatalf("an unreferenced file was backed up: %v", err)
	}
	if _, err := Restore(context.Background(), out, restored); err != nil {
		t.Fatalf("restoring again over identical files: %v", err)
	}
}

func TestARestoreRefusesADamagedFile(t *testing.T) {
	dir, kept, _ := blobDataDir(t)
	out := filepath.Join(t.TempDir(), "backup")
	if _, err := Backup(context.Background(), dir, out, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blobAt(out, kept[1]), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	restored := t.TempDir()
	if _, err := Restore(context.Background(), out, restored); err == nil || !strings.Contains(err.Error(), "does not match its manifest") {
		t.Fatalf("restore of a damaged file: %v", err)
	}
	if entries, _ := os.ReadDir(restored); len(entries) != 0 {
		t.Fatalf("a refused restore wrote %d entries", len(entries))
	}
}

func TestABackupRefusesAMissingReferencedFile(t *testing.T) {
	dir, kept, _ := blobDataDir(t)
	if err := os.Remove(blobAt(dir, kept[0])); err != nil {
		t.Fatal(err)
	}
	_, err := Backup(context.Background(), dir, filepath.Join(t.TempDir(), "backup"), nil)
	if err == nil || !strings.Contains(err.Error(), kept[0].SHA256) || !strings.Contains(err.Error(), "team/app") {
		t.Fatalf("backup with a missing file: %v", err)
	}
}
