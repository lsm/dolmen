package dolmen

import (
	"errors"
	"reflect"
	"testing"
)

func TestImportRecordsKeepsSourceIdentity(t *testing.T) {
	st, ctx := openWithNotes(t)
	rows := []map[string]any{
		{"id": int64(9007199254740993), "created_at": "2026-01-02T03:04:05.123Z", "title": "granite"},
		{"id": int64(42), "created_at": "2026-01-02T03:04:05.124Z", "title": "basalt"},
	}
	got, err := st.ImportRecords(ctx, "app", "notes", rows, ImportOptions{IdempotencyKey: "snap/notes/1"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Inserted != 2 || got.Skipped != 0 || got.Changes.Count != 2 || !reflect.DeepEqual(got.Ids, []int64{9007199254740993, 42}) {
		t.Fatalf("import: %+v", got)
	}
	read, err := st.GetRows(ctx, "app", "notes", []int64{42})
	if err != nil || len(read.Rows) != 1 || read.Rows[0]["created_at"] != "2026-01-02T03:04:05.124Z" {
		t.Fatalf("read: %+v %v", read, err)
	}
	replay, err := st.ImportRecords(ctx, "app", "notes", rows, ImportOptions{IdempotencyKey: "snap/notes/1"})
	if err != nil || !replay.Replayed || replay.Inserted != 2 {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	resumed, err := st.ImportRecords(ctx, "app", "notes", rows[1:], ImportOptions{IdempotencyKey: "snap/notes/regrouped"})
	if err != nil || resumed.Skipped != 1 || resumed.Changes.Count != 0 {
		t.Fatalf("resume: %+v %v", resumed, err)
	}
	changed := []map[string]any{{"id": int64(42), "created_at": "2026-01-02T03:04:05.124Z", "title": "changed"}}
	if _, err := st.ImportRecords(ctx, "app", "notes", changed, ImportOptions{IdempotencyKey: "snap/notes/2"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("differing content: %v", err)
	}
	if _, err := st.ImportRecords(ctx, "app", "notes", rows, ImportOptions{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("missing key: %v", err)
	}
	if _, err := st.ImportRecords(ctx, "absent", "notes", rows, ImportOptions{IdempotencyKey: "k"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("import must not create a namespace: %v", err)
	}
	next, err := st.Insert(ctx, "app", "notes", []map[string]any{{"title": "ordinary"}}, InsertOptions{})
	if err != nil || next.Ids[0] <= 9007199254740993 {
		t.Fatalf("allocator: %+v %v", next, err)
	}
}
