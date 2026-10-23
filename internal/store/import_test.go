package store

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func importFixture(t *testing.T) *Store {
	t.Helper()
	st := openStore(t)
	mustNS(t, st, "test")
	if _, err := st.CreateTable(context.Background(), "test", "events", []schema.Field{
		{Name: "body", Type: schema.Text, Fulltext: true},
		{Name: "active", Type: schema.Boolean},
		{Name: "meta", Type: schema.JSON},
	}); err != nil {
		t.Fatal(err)
	}
	return st.Store
}

func sourceRow(id any, body string) map[string]any {
	return map[string]any{"id": id, "created_at": "2026-01-02T03:04:05.123Z", "body": body, "active": true, "meta": map[string]any{"n": json.Number("7")}}
}

func TestImportPreservesIdentityAndResumes(t *testing.T) {
	st, ctx := importFixture(t), context.Background()
	rows := []map[string]any{sourceRow(json.Number("9007199254740993"), "stone memory"), sourceRow(int64(42), "second")}
	got, err := st.ImportRecords(ctx, "test", "events", rows, WriteOpts{IdempotencyKey: "chunk1"}, Embedder{}, Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Inserted != 2 || got.Skipped != 0 || !reflect.DeepEqual(got.Ids, []int64{9007199254740993, 42}) {
		t.Fatalf("import: %+v", got)
	}
	read, err := st.GetRows(ctx, "test", "events", []int64{42, 9007199254740993}, nil, Incarnation{})
	if err != nil || len(read.Rows) != 2 || read.Rows[0]["created_at"] != "2026-01-02T03:04:05.123Z" {
		t.Fatalf("read: %+v %v", read, err)
	}
	replay, err := st.ImportRecords(ctx, "test", "events", rows, WriteOpts{IdempotencyKey: "chunk1"}, Embedder{}, Incarnation{})
	if err != nil || !replay.Replayed || replay.Inserted != 2 {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	retry, err := st.ImportRecords(ctx, "test", "events", rows[1:], WriteOpts{IdempotencyKey: "regrouped"}, Embedder{}, Incarnation{})
	if err != nil || retry.Skipped != 1 || retry.Inserted != 0 || retry.Changes.Count != 0 {
		t.Fatalf("resume: %+v %v", retry, err)
	}
	next, err := st.Insert(ctx, "test", "events", []map[string]any{{"body": "ordinary"}}, WriteOpts{IdempotencyKey: "chunk1"}, Embedder{}, nil, Incarnation{})
	if err != nil || next.Ids[0] <= 9007199254740993 {
		t.Fatalf("allocator or receipt domain: %+v %v", next, err)
	}
	if _, err = st.Insert(ctx, "test", "events", rows, WriteOpts{}, Embedder{}, nil, Incarnation{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("normal insert accepted metadata: %v", err)
	}
}

func TestImportConflictRollsBackChunk(t *testing.T) {
	st, ctx := importFixture(t), context.Background()
	if _, err := st.ImportRecords(ctx, "test", "events", []map[string]any{sourceRow(42, "original")}, WriteOpts{IdempotencyKey: "one"}, Embedder{}, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	_, err := st.ImportRecords(ctx, "test", "events", []map[string]any{sourceRow(43, "new"), sourceRow(42, "different")}, WriteOpts{IdempotencyKey: "two"}, Embedder{}, Incarnation{})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict: %v", err)
	}
	read, err := st.GetRows(ctx, "test", "events", []int64{42, 43}, nil, Incarnation{})
	if err != nil || len(read.Rows) != 1 || read.Rows[0]["body"] != "original" {
		t.Fatalf("partial write: %+v %v", read, err)
	}
	_, err = st.ImportRecords(ctx, "test", "events", []map[string]any{sourceRow(44, "changed")}, WriteOpts{IdempotencyKey: "one"}, Embedder{}, Incarnation{})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("key reuse: %v", err)
	}
}

func TestImportRejectsInvalidMetadataAtomically(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"missing id":      func(r map[string]any) { delete(r, "id") },
		"zero":            func(r map[string]any) { r["id"] = 0 },
		"fraction":        func(r map[string]any) { r["id"] = 1.5 },
		"overflow":        func(r map[string]any) { r["id"] = json.Number("9223372036854775808") },
		"max":             func(r map[string]any) { r["id"] = int64(math.MaxInt64) },
		"imprecise float": func(r map[string]any) { r["id"] = float64(9007199254740994) },
		"microseconds":    func(r map[string]any) { r["created_at"] = "2026-01-02T03:04:05.123456Z" },
		"offset":          func(r map[string]any) { r["created_at"] = "2026-01-02T04:04:05.123+01:00" },
		"unknown":         func(r map[string]any) { r["unknown"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			st := importFixture(t)
			row := sourceRow(42, "bad")
			mutate(row)
			_, err := st.ImportRecords(context.Background(), "test", "events", []map[string]any{sourceRow(41, "valid"), row}, WriteOpts{IdempotencyKey: "bad"}, Embedder{}, Incarnation{})
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid input: %v", err)
			}
			read, err := st.GetRows(context.Background(), "test", "events", []int64{41, 42}, nil, Incarnation{})
			if err != nil || len(read.Rows) != 0 {
				t.Fatalf("partial validation: %+v %v", read, err)
			}
		})
	}
}
