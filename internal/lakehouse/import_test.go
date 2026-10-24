package lakehouse

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/secret"
	"github.com/lsm/dolmen/internal/store"
)

const importStamp = "2026-01-02T03:04:05.123Z"

func importStore(t *testing.T, opts ...OpenOption) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.CreateNamespace(t.Context(), "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	fields := []schema.Field{{Name: "body", Type: schema.Text}, {Name: "active", Type: schema.Boolean}, {Name: "meta", Type: schema.JSON}, {Name: "score", Type: schema.Number}}
	if _, err := s.CreateTable(t.Context(), "app", "events", fields, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	return s
}

func importRow(id any, body string) map[string]any {
	return map[string]any{"id": id, "created_at": importStamp, "body": body, "active": true, "meta": map[string]any{"n": json.Number("7"), "a": "b"}, "score": json.Number("1.5")}
}

func importKey(key string) store.WriteOpts { return store.WriteOpts{IdempotencyKey: key} }

func TestImportPreservesIdentityAndResumes(t *testing.T) {
	s := importStore(t)
	ctx := t.Context()
	rows := []map[string]any{importRow(json.Number("9007199254740993"), "stone memory"), importRow(int64(42), "second")}
	got, err := s.ImportRecords(ctx, "app", "events", rows, importKey("chunk1"), store.Embedder{}, store.Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Inserted != 2 || got.Skipped != 0 || got.Changes.Count != 2 || !reflect.DeepEqual(got.Ids, []int64{9007199254740993, 42}) {
		t.Fatalf("import: %+v", got)
	}
	read, err := s.GetRows(ctx, "app", "events", []int64{42, 9007199254740993}, nil, store.Incarnation{})
	if err != nil || len(read.Rows) != 2 || read.Rows[0]["created_at"] != importStamp || read.Rows[1]["id"] != int64(9007199254740993) {
		t.Fatalf("read: %+v %v", read, err)
	}
	replay, err := s.ImportRecords(ctx, "app", "events", rows, importKey("chunk1"), store.Embedder{}, store.Incarnation{})
	if err != nil || !replay.Replayed || replay.Inserted != 2 {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	retry, err := s.ImportRecords(ctx, "app", "events", []map[string]any{rows[1], importRow(43, "third")}, importKey("regrouped"), store.Embedder{}, store.Incarnation{})
	if err != nil || retry.Skipped != 1 || retry.Inserted != 1 || retry.Changes.Count != 1 {
		t.Fatalf("resume: %+v %v", retry, err)
	}
	feed, _, err := s.ChangesSince(ctx, "app", "events", store.CursorBegin, [16]byte{}, nil, store.Incarnation{}, store.Page{})
	if err != nil || len(feed) != 3 {
		t.Fatalf("skips must emit no change events: %+v %v", feed, err)
	}
	next, err := s.Insert(ctx, "app", "events", []map[string]any{{"body": "ordinary"}}, importKey("chunk1"), store.Embedder{}, nil, store.Incarnation{})
	if err != nil || next.Replayed || next.Ids[0] <= 9007199254740993 {
		t.Fatalf("allocator or receipt domain: %+v %v", next, err)
	}
	if _, err := s.Insert(ctx, "app", "events", rows, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("ordinary insert accepted system columns: %v", err)
	}
}

func TestImportConflictRollsBackChunk(t *testing.T) {
	s := importStore(t)
	ctx := t.Context()
	if _, err := s.ImportRecords(ctx, "app", "events", []map[string]any{importRow(42, "original")}, importKey("one"), store.Embedder{}, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	_, err := s.ImportRecords(ctx, "app", "events", []map[string]any{importRow(43, "new"), importRow(42, "different")}, importKey("two"), store.Embedder{}, store.Incarnation{})
	if !errors.Is(err, derr.ErrConflict) || err.Error() != "import record 1 differs from the existing id; use a fresh target or correct the source" {
		t.Fatalf("conflict: %v", err)
	}
	read, err := s.GetRows(ctx, "app", "events", []int64{42, 43}, nil, store.Incarnation{})
	if err != nil || len(read.Rows) != 1 || read.Rows[0]["body"] != "original" {
		t.Fatalf("partial write: %+v %v", read, err)
	}
	if _, err := s.ImportRecords(ctx, "app", "events", []map[string]any{importRow(44, "changed")}, importKey("one"), store.Embedder{}, store.Incarnation{}); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("key reuse: %v", err)
	}
	if err := s.DropTable(ctx, "app", "events", store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "events", []schema.Field{{Name: "body"}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.ImportRecords(ctx, "app", "events", []map[string]any{{"id": 42, "created_at": importStamp, "body": "new lifetime"}}, importKey("one"), store.Embedder{}, store.Incarnation{})
	if err != nil || fresh.Replayed || fresh.Inserted != 1 {
		t.Fatalf("receipt survived drop: %+v %v", fresh, err)
	}
}

func TestImportAllocatorUnderConcurrentWrites(t *testing.T) {
	s := importStore(t)
	ctx := t.Context()
	var wg sync.WaitGroup
	failures := make(chan error, 12)
	for i := range 12 {
		wg.Go(func() {
			var err error
			if i%3 == 0 {
				_, err = s.ImportRecords(ctx, "app", "events", []map[string]any{importRow(int64(1000*(i+1)), fmt.Sprint(i))}, importKey(fmt.Sprint("chunk-", i)), store.Embedder{}, store.Incarnation{})
			} else {
				_, err = s.Insert(ctx, "app", "events", []map[string]any{{"body": "ordinary"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{})
			}
			if err != nil {
				failures <- err
			}
		})
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if _, err := s.ImportRecords(ctx, "app", "events", []map[string]any{importRow(int64(500), "below high-water")}, importKey("low"), store.Embedder{}, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	next, err := s.Insert(ctx, "app", "events", []map[string]any{{"body": "after"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{})
	if err != nil || next.Ids[0] <= 10000 {
		t.Fatalf("allocator moved backwards: %+v %v", next, err)
	}
	if _, count, err := s.DescribeTable(ctx, "app", "events", nil, store.Incarnation{}); err != nil || count != 14 {
		t.Fatalf("rows: %d %v", count, err)
	}
}

func TestImportSecretsVectorsAndDefaultsSkipByContent(t *testing.T) {
	key, err := secret.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s := importStore(t, WithSecretKeyring(key))
	ctx := t.Context()
	fields := []schema.Field{{Name: "token", Type: schema.Secret}, {Name: "stamp", Type: schema.Timestamp, Default: schema.NowDefault}, {Name: "flag", Type: schema.Boolean, Default: true}, {Name: "emb", Type: schema.Vector, Dim: 3}}
	if _, err := s.CreateTable(ctx, "app", "vault", fields, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	row := map[string]any{"id": 42, "created_at": importStamp, "token": "synthetic-value", "emb": []any{1, 0, 0}}
	for _, k := range []string{"one", "two"} {
		got, err := s.ImportRecords(ctx, "app", "vault", []map[string]any{row}, importKey(k), store.Embedder{}, store.Incarnation{})
		if err != nil || (k == "two" && got.Skipped != 1) {
			t.Fatalf("secret/default skip: %+v %v", got, err)
		}
	}
	read, err := s.GetRows(ctx, "app", "vault", []int64{42}, nil, store.Incarnation{})
	if err != nil || len(read.Rows) != 1 || read.Rows[0]["stamp"] != importStamp {
		t.Fatalf("now default must use created_at: %+v %v", read, err)
	}
	row["token"] = "different-synthetic-value"
	if _, err := s.ImportRecords(ctx, "app", "vault", []map[string]any{row}, importKey("three"), store.Embedder{}, store.Incarnation{}); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("secret conflict: %v", err)
	}
}

func TestImportNamesTheFailingRecord(t *testing.T) {
	s := importStore(t)
	ctx := t.Context()
	bad := importRow(43, "bad")
	bad["bogus"] = true
	_, err := s.ImportRecords(ctx, "app", "events", []map[string]any{importRow(42, "fine"), bad}, importKey("bad"), store.Embedder{}, store.Incarnation{})
	if !errors.Is(err, store.ErrInvalid) || err.Error() != `import record 1: invalid request: unknown field "bogus" on table events (see describe_table)` {
		t.Fatalf("remediation: %v", err)
	}
	read, err := s.GetRows(ctx, "app", "events", []int64{42}, nil, store.Incarnation{})
	if err != nil || len(read.Rows) != 0 {
		t.Fatalf("partial validation: %+v %v", read, err)
	}
}
