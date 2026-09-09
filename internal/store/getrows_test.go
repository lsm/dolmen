package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func mustGetRowsNotes(t *testing.T) legacyStore {
	t.Helper()
	st := openStore(t)
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)
	return st
}

// TestGetRowsReturnsRequestedIDs pins the read_rows seam contract: the ids
// address a set — each found row appears once, in ascending id order — and
// ids that are missing are simply absent, never an error (§2).
func TestGetRowsReturnsRequestedIDs(t *testing.T) {
	st := mustGetRowsNotes(t)
	ctx := context.Background()

	res, err := st.GetRows(ctx, "test", "notes", []int64{3, 999, 1}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("GetRows: %v", err)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("expected the two found rows, got %d: %v", len(res.Rows), res.Rows)
	}
	if res.Rows[0]["id"] != int64(1) || res.Rows[1]["id"] != int64(3) {
		t.Fatalf("rows must come back in ascending id order regardless of request order: %v", res.Rows)
	}
	if res.Rows[0]["title"] != "first note" || res.Rows[1]["title"] != "third note" {
		t.Fatalf("rows must carry their fields: %v", res.Rows)
	}
	if res.Truncated {
		t.Fatalf("a page under the response budget must not report truncated: %v", res)
	}
}

// TestGetRowsBudgetTruncation pins the truncation signal (§6.2's projected
// response-byte budget): rows that exist but would exceed the budget are
// dropped from the page with Truncated=true — the caller can tell a dropped
// existing row from an absent id, which is the whole point of the flag.
func TestGetRowsBudgetTruncation(t *testing.T) {
	st := openStore(t)
	mustNS(t, st, "test")
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "bigrows", []schema.Field{
		{Name: "v", Type: schema.Text},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	chunk := strings.Repeat("x", 12<<20)
	for i := 0; i < 4; i++ {
		if _, err := st.Insert(ctx, "test", "bigrows", []map[string]any{{"v": chunk}}, testEmbed); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	res, err := st.GetRows(ctx, "test", "bigrows", []int64{1, 2, 3, 4}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("GetRows: %v", err)
	}
	if len(res.Rows) != 2 || !res.Truncated {
		t.Fatalf("byte budget should cap the page at 2 of 4 12MiB rows with truncated=true, got %d rows truncated=%v", len(res.Rows), res.Truncated)
	}
}

// TestGetRowsCollapsesDuplicates: a repeated id names one row; the response
// never repeats it.
func TestGetRowsCollapsesDuplicates(t *testing.T) {
	st := mustGetRowsNotes(t)

	res, err := st.GetRows(context.Background(), "test", "notes", []int64{2, 1, 2}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("GetRows: %v", err)
	}
	if len(res.Rows) != 2 || res.Rows[0]["id"] != int64(1) || res.Rows[1]["id"] != int64(2) {
		t.Fatalf("duplicate ids must collapse to one row each, ascending: %v", res.Rows)
	}
}

// TestGetRowsIDCap pins §2's per-request cap: at most MaxReadRowsIDs ids,
// invalid_request beyond — a bounded body can still name unbounded ids.
func TestGetRowsIDCap(t *testing.T) {
	st := mustGetRowsNotes(t)
	ctx := context.Background()

	atCap := make([]int64, MaxReadRowsIDs)
	for i := range atCap {
		atCap[i] = int64(i + 1)
	}
	res, err := st.GetRows(ctx, "test", "notes", atCap, nil, Incarnation{})
	if err != nil {
		t.Fatalf("GetRows at the %d-id cap: %v", MaxReadRowsIDs, err)
	}
	// Only the three seeded rows exist; the rest of the ids are absent.
	if len(res.Rows) != 3 {
		t.Fatalf("expected the three seeded rows, got %d", len(res.Rows))
	}

	over := append(atCap, MaxReadRowsIDs+1)
	if _, err := st.GetRows(ctx, "test", "notes", over, nil, Incarnation{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("GetRows beyond the cap must be invalid_request, got %v", err)
	}
}

// TestGetRowsEmptyIDs: an empty id set is a well-formed no-op — the table
// must still exist (the engine never creates implicitly), and the response
// is an empty page, not nil.
func TestGetRowsEmptyIDs(t *testing.T) {
	st := mustGetRowsNotes(t)

	res, err := st.GetRows(context.Background(), "test", "notes", nil, nil, Incarnation{})
	if err != nil {
		t.Fatalf("GetRows with no ids: %v", err)
	}
	if res.Rows == nil || len(res.Rows) != 0 {
		t.Fatalf("empty id set must return an empty, non-nil page: %v", res.Rows)
	}

	if _, err := st.GetRows(context.Background(), "test", "nope", nil, nil, Incarnation{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRows on a missing table must be not_found, got %v", err)
	}
	if _, err := st.GetRows(context.Background(), "absent", "notes", []int64{1}, nil, Incarnation{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRows on a missing namespace must be not_found, got %v", err)
	}
}

// TestGetRowsHidesEmbedding: the projection is the typed read's — the hidden
// _embedding column of a vectorized table never surfaces, and declared field
// types decode exactly as query results decode them.
func TestGetRowsHidesEmbedding(t *testing.T) {
	st := mustGetRowsNotes(t)

	res, err := st.GetRows(context.Background(), "test", "notes", []int64{1}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("GetRows: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("expected one row, got %d", len(res.Rows))
	}
	row := res.Rows[0]
	if _, ok := row["_embedding"]; ok {
		t.Fatalf("the hidden _embedding column must not surface: %v", row)
	}
	if row["done"] != true {
		t.Fatalf("boolean field must decode as bool, got %T %v", row["done"], row["done"])
	}
	if vec, ok := row["emb"].([]float64); !ok || len(vec) != 4 {
		t.Fatalf("vector field must decode as []float64, got %T %v", row["emb"], row["emb"])
	}
}
