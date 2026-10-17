package lakehouse

import (
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
