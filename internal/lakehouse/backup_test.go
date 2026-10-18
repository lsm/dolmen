package lakehouse

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func backupSource(t *testing.T) (string, *Store) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "team/app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "team/app", "notes", []schema.Field{{Name: "title", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	insert := func(titles ...string) {
		var recs []map[string]any
		for _, title := range titles {
			recs = append(recs, map[string]any{"title": title})
		}
		if _, err := s.Insert(ctx, "team/app", "notes", recs, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
			t.Fatal(err)
		}
	}
	insert("a", "b")
	insert("c")
	if _, err := s.Vacuum(ctx, "team/app"); err != nil {
		t.Fatal(err)
	}
	insert("d", "e")
	return dir, s
}

func TestALakehouseBackupRestoresWhatTheServerHeld(t *testing.T) {
	dir, s := backupSource(t)
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte("x")))
	blob := filepath.Join(dir, "team", "app.files", sum[:2], sum)
	os.MkdirAll(filepath.Dir(blob), 0o700)
	os.WriteFile(blob, []byte("x"), 0o600)
	out := filepath.Join(t.TempDir(), "backup")
	m, err := Backup(t.Context(), dir, out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Engine != store.EngineLakehouse || m.DataDir != dir || len(m.DataFiles) == 0 || len(m.Blobs) != 1 {
		t.Fatalf("manifest = %+v", m)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "team")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Restore(t.Context(), out, dir); err != nil {
		t.Fatal(err)
	}
	r := openStore(t, dir)
	res, err := r.GetRows(t.Context(), "team/app", "notes", []int64{1, 2, 3, 4, 5}, nil, store.Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 5 {
		t.Fatalf("restored %d rows, want 5: %v", len(res.Rows), res.Rows)
	}
	if got, err := os.ReadFile(blob); err != nil || string(got) != "x" {
		t.Fatalf("restored file = %q, %v", got, err)
	}
}

func TestALakehouseBackupWaitsOutAVacuum(t *testing.T) {
	dir, s := backupSource(t)
	defer s.Close()
	attempts := backupAttempts
	backupAttempts = 1
	defer func() { backupAttempts = attempts }()
	if err := s.stampVacuum("team/app", false); err != nil {
		t.Fatal(err)
	}
	_, err := Backup(t.Context(), dir, filepath.Join(t.TempDir(), "backup"), nil)
	if err == nil || !strings.Contains(err.Error(), "vacuum") || !strings.Contains(err.Error(), "team/app") {
		t.Fatalf("backup during a vacuum: %v", err)
	}
	if err := s.stampVacuum("team/app", true); err != nil {
		t.Fatal(err)
	}
	if _, err := Backup(t.Context(), dir, filepath.Join(t.TempDir(), "backup"), nil); err != nil {
		t.Fatalf("backup after the vacuum: %v", err)
	}
}

func TestALakehouseBackupRefusesAMissingReferencedFile(t *testing.T) {
	dir, s := backupSource(t)
	defer s.Close()
	attempts := backupAttempts
	backupAttempts = 1
	defer func() { backupAttempts = attempts }()
	var victim string
	filepath.WalkDir(filepath.Join(dir, "team", "app.lakehouse", "data"), func(path string, d fs.DirEntry, err error) error {
		if err == nil && victim == "" && strings.HasSuffix(path, ".avro") {
			victim = path
		}
		return err
	})
	if victim == "" {
		t.Fatal("no manifest file to remove")
	}
	if err := os.Remove(victim); err != nil {
		t.Fatal(err)
	}
	_, err := Backup(t.Context(), dir, filepath.Join(t.TempDir(), "backup"), nil)
	if err == nil || !strings.Contains(err.Error(), "rerun the backup") {
		t.Fatalf("backup with a missing referenced file: %v", err)
	}
}

func TestALakehouseBackupIsConsistentWhileWritesAndVacuumsContinue(t *testing.T) {
	dir, s := backupSource(t)
	ctx := t.Context()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			s.Insert(ctx, "team/app", "notes", []map[string]any{{"title": "during"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{})
			if i%10 == 0 && i < 30 {
				if _, err := s.Vacuum(ctx, "team/app"); err != nil {
					t.Errorf("vacuum: %v", err)
					return
				}
			}
		}
	}()
	out := filepath.Join(t.TempDir(), "backup")
	attempts := backupAttempts
	backupAttempts = 50
	defer func() { backupAttempts = attempts }()
	_, err := Backup(ctx, dir, out, nil)
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(dir, "team"))
	if _, err := store.Restore(ctx, out, dir); err != nil {
		t.Fatal(err)
	}
	r := openStore(t, dir)
	res, err := r.GetRows(ctx, "team/app", "notes", []int64{1, 2, 3, 4, 5}, nil, store.Incarnation{})
	if err != nil || len(res.Rows) != 5 {
		t.Fatalf("restored rows = %v, %v", res.Rows, err)
	}
	if _, err := r.Vacuum(ctx, "team/app"); err != nil {
		t.Fatalf("vacuum reads every restored file: %v", err)
	}
}

func TestALakehouseBackupRetriesATornVacuumStamp(t *testing.T) {
	dir, s := backupSource(t)
	defer s.Close()
	attempts := backupAttempts
	backupAttempts = 1
	defer func() { backupAttempts = attempts }()
	if err := os.WriteFile(filepath.Join(dir, "team", "app.lakehouse", vacuumStamp), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Backup(t.Context(), dir, filepath.Join(t.TempDir(), "backup"), nil)
	if err == nil || strings.Contains(err.Error(), "ParseInt") || !strings.Contains(err.Error(), "rerun the backup") {
		t.Fatalf("backup over a half-written stamp: %v", err)
	}
}

func TestALakehouseBackupRefusesABlobThatDoesNotHashToItsName(t *testing.T) {
	dir, s := backupSource(t)
	defer s.Close()
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte("x")))
	blob := filepath.Join(dir, "team", "app.files", sum[:2], sum)
	os.MkdirAll(filepath.Dir(blob), 0o700)
	os.WriteFile(blob, []byte("rotted"), 0o600)
	_, err := Backup(t.Context(), dir, filepath.Join(t.TempDir(), "backup"), nil)
	if err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("backup with a damaged blob: %v", err)
	}
}

func TestALakehouseBackupRestoresANamespaceWithoutTables(t *testing.T) {
	dir, s := backupSource(t)
	if err := s.CreateNamespace(t.Context(), "empty", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "backup")
	if _, err := Backup(t.Context(), dir, out, nil); err != nil {
		t.Fatal(err)
	}
	s.Close()
	os.RemoveAll(filepath.Join(dir, "team"))
	os.RemoveAll(filepath.Join(dir, "empty.lakehouse"))
	if _, err := store.Restore(t.Context(), out, dir); err != nil {
		t.Fatal(err)
	}
	r := openStore(t, dir)
	if _, err := r.CreateTable(t.Context(), "empty", "notes", []schema.Field{{Name: "title", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("use the restored empty namespace: %v", err)
	}
}

func TestALakehouseBackupLeavesOutANamespaceDroppedWhileCopied(t *testing.T) {
	dir, s := backupSource(t)
	defer s.Close()
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte("x")))
	blob := filepath.Join(dir, "team", "app.files", sum[:2], sum)
	os.MkdirAll(filepath.Dir(blob), 0o700)
	os.WriteFile(blob, []byte("x"), 0o600)
	beforeBlobCopy = func(ns string) {
		if err := s.DropNamespace(t.Context(), ns, [16]byte{}); err != nil {
			t.Error(err)
		}
		os.RemoveAll(filepath.Join(dir, "team", "app.files"))
	}
	defer func() { beforeBlobCopy = nil }()
	m, err := Backup(t.Context(), dir, filepath.Join(t.TempDir(), "backup"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range m.Files {
		if f.Namespace == "team/app" {
			t.Fatalf("a namespace dropped during its copy was backed up: %+v", m)
		}
	}
}

func TestALakehouseBackupHandlesSymlinksAndURIPathCharacters(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "data #1?x=%20")
	s := openStore(t, dir)
	if err := s.CreateNamespace(t.Context(), "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skip(err)
	}
	if names, err := NamespacesIn(link); err != nil || len(names) != 1 {
		t.Fatalf("NamespacesIn through a symlink = %v, %v", names, err)
	}
	m, err := Backup(t.Context(), link, filepath.Join(t.TempDir(), "backup"), nil)
	if err != nil || len(m.Files) != 1 {
		t.Fatalf("backup of %s = %+v, %v", dir, m, err)
	}
}

func TestALakehouseBackupIgnoresHiddenFilesBesideNamespaces(t *testing.T) {
	dir, s := backupSource(t)
	defer s.Close()
	for _, hidden := range []string{".DS_Store", filepath.Join("team", ".DS_Store"), filepath.Join("team", "app.lakehouse", "data", ".swp")} {
		if err := os.WriteFile(filepath.Join(dir, hidden), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if names, err := NamespacesIn(dir); err != nil || len(names) != 1 || names[0] != "team/app" {
		t.Fatalf("NamespacesIn beside hidden files = %v, %v", names, err)
	}
	m, err := Backup(t.Context(), dir, filepath.Join(t.TempDir(), "backup"), nil)
	if err != nil || len(m.Files) != 1 || len(m.Dirs) == 0 {
		t.Fatalf("backup beside hidden files = %+v, %v", m, err)
	}
}

type hashedFiles map[string]store.FileBlob

func (f hashedFiles) Describe(part string) (store.FileBlob, error) { return f[part], nil }
func (f hashedFiles) Attach(_ context.Context, _, part string) (store.FileBlob, error) {
	return f[part], nil
}
func (f hashedFiles) Pin(_, _ string) error                         { return nil }
func (f hashedFiles) Fetch(context.Context, string) (string, error) { return "", nil }

func TestALakehouseBackupRequiresEveryReferencedBlob(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := openStore(t, dir)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "docs", []schema.Field{{Name: "doc", Type: schema.File}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	src := hashedFiles{}
	paths := map[string]string{}
	for _, part := range []string{"stored", "pending"} {
		sum := fmt.Sprintf("%x", sha256.Sum256([]byte(part)))
		src[part] = store.FileBlob{SHA256: sum, Size: int64(len(part)), MediaType: "text/plain"}
		paths[part] = filepath.Join(dir, "app.files", sum[:2], sum)
		os.MkdirAll(filepath.Dir(paths[part]), 0o700)
		os.WriteFile(paths[part], []byte(part), 0o600)
	}
	insert := func(part string) {
		if _, err := s.Insert(store.WithFileSource(ctx, src), "app", "docs", []map[string]any{{"doc": map[string]any{"part": part}}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
			t.Fatal(err)
		}
	}
	insert("stored")
	if _, err := s.GetRows(ctx, "app", "docs", []int64{1}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	insert("pending")
	if _, err := Backup(ctx, dir, filepath.Join(t.TempDir(), "backup"), nil); err != nil {
		t.Fatalf("backup with every blob present: %v", err)
	}
	attempts := backupAttempts
	backupAttempts = 1
	defer func() { backupAttempts = attempts }()
	for _, part := range []string{"pending", "stored"} {
		if err := os.Remove(paths[part]); err != nil {
			t.Fatal(err)
		}
		_, err := Backup(ctx, dir, filepath.Join(t.TempDir(), "backup"), nil)
		if err == nil || !strings.Contains(err.Error(), src[part].SHA256) {
			t.Fatalf("backup without the %s row's blob: %v", part, err)
		}
		os.WriteFile(paths[part], []byte(part), 0o600)
	}
}

func TestALakehouseBackupLeavesOutANamespaceDroppedBeforeItsSnapshot(t *testing.T) {
	dir, s := backupSource(t)
	defer s.Close()
	beforeSnapshot = func(ns string) {
		if err := s.DropNamespace(t.Context(), ns, [16]byte{}); err != nil {
			t.Error(err)
		}
	}
	defer func() { beforeSnapshot = nil }()
	m, err := Backup(t.Context(), dir, filepath.Join(t.TempDir(), "backup"), nil)
	if err != nil || len(m.Files) != 0 {
		t.Fatalf("backup after a drop = %+v, %v", m, err)
	}
}
