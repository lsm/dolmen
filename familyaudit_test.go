package dolmen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/lsm/dolmen/internal/store"
)

func TestCreateTableRejectsTooManyFields(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fields := make([]Field, store.MaxFieldsPerTable+1)
	for i := range fields {
		fields[i] = Field{Name: fmt.Sprintf("f%03d", i), Type: Number}
	}
	if _, err := st.CreateTable(context.Background(), "fa", "wide", fields); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("oversized field list must be rejected invalid_request (max %d), got %v", store.MaxFieldsPerTable, err)
	}
}

func TestInsertRejectsTooManyRecords(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "fa", "notes", []Field{{Name: "body", Type: Text}}); err != nil {
		t.Fatal(err)
	}
	records := make([]map[string]any, store.MaxRecordsPerInsert+1)
	for i := range records {
		records[i] = map[string]any{"body": "r"}
	}
	if _, err := st.Insert(ctx, "fa", "notes", records, InsertOptions{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("oversized record batch must be rejected invalid_request (max %d), got %v", store.MaxRecordsPerInsert, err)
	}
	if _, err := st.UpsertByKey(ctx, "fa", "notes", []string{"body"}, records); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("oversized upsert batch must be rejected invalid_request (max %d), got %v", store.MaxRecordsPerInsert, err)
	}
}

func TestInsertRejectsNonFiniteNumbers(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "fa", "nums", []Field{{Name: "scalar", Type: Number}}); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]any{"NaN": math.NaN(), "Inf": math.Inf(1), "NegInf": math.Inf(-1), "NaN32": float32(math.NaN()), "jsonNaN": json.Number("NaN"), "jsonInf": json.Number("Inf")} {
		if _, err := st.Insert(ctx, "fa", "nums", []map[string]any{{"scalar": v}}, InsertOptions{}); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("%s must be rejected invalid_request on write (NaN reads back as NULL, infinities poison the row), got %v", name, err)
		}
	}
	if _, err := st.Update(ctx, "fa", "nums", UpdateOptions{Filter: "1=1", Set: map[string]any{"scalar": math.Inf(1)}}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Update Set carrying an infinity must be rejected invalid_request, got %v", err)
	}
	if _, err := st.Insert(ctx, "fa", "nums", []map[string]any{{"scalar": 2.5}}, InsertOptions{}); err != nil {
		t.Fatalf("finite numbers must still store: %v", err)
	}
}

func TestDescribeDropRejectInvalidTableName(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	for _, bad := range []string{"sqlite_notes", "notes__fts5", "has space", ""} {
		if _, _, err := st.DescribeTable(ctx, "fa", bad); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("DescribeTable(%q) must be rejected invalid_request (matching the wire's table grammar), got %v", bad, err)
		}
		if err := st.DropTable(ctx, "fa", bad); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("DropTable(%q) must be rejected invalid_request (matching the wire's table grammar), got %v", bad, err)
		}
	}
}

func TestInsertRejectsNilRecord(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "fa", "notes", []Field{{Name: "body", Type: Text, Required: true}}); err != nil {
		t.Fatal(err)
	}
	res, err := st.Insert(ctx, "fa", "notes", []map[string]any{nil, {"body": "real"}}, InsertOptions{})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a nil record must be rejected as the wire rejects it, got %v (ids %v)", err, res.Ids)
	}
	rows, err := st.Query(ctx, "fa", "SELECT id FROM notes", QueryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Rows) != 0 {
		t.Fatalf("a rejected batch must insert nothing, got %d rows", len(rows.Rows))
	}
}
