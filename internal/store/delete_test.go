package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func TestDeleteDryRunCountsWithoutDeleting(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	res, err := st.Delete(ctx, "test", "notes", "1=1", nil, DeleteOptions{DryRun: true})
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if res.Matched != 3 {
		t.Fatalf("expected matched 3, got %d", res.Matched)
	}
	if res.Deleted != 0 {
		t.Fatalf("expected deleted 0 in dry-run, got %d", res.Deleted)
	}

	rows, _, err := st.Query(ctx, "test", "SELECT count(*) AS n FROM notes", nil)
	if err != nil {
		t.Fatalf("count after dry-run: %v", err)
	}
	if rows[0]["n"].(int64) != 3 {
		t.Fatalf("dry-run must not delete rows, got %d", rows[0]["n"])
	}
}

func TestDeleteReturnsMatchedAndDeleted(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	res, err := st.Delete(ctx, "test", "notes", "done = 1", nil, DeleteOptions{})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if res.Matched != 1 || res.Deleted != 1 {
		t.Fatalf("expected matched=1 deleted=1, got matched=%d deleted=%d", res.Matched, res.Deleted)
	}
}

func TestDeleteLimitAllowsBelowThreshold(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	// 3 rows match 1=1, limit 10 is above the match count.
	res, err := st.Delete(ctx, "test", "notes", "1=1", nil, DeleteOptions{Limit: 10})
	if err != nil {
		t.Fatalf("delete below limit: %v", err)
	}
	if res.Matched != 3 || res.Deleted != 3 {
		t.Fatalf("expected matched=3 deleted=3, got matched=%d deleted=%d", res.Matched, res.Deleted)
	}
}

func TestDeleteLimitRequiresConfirm(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	// 3 rows match, limit 1 is below the match count. Without confirm it must fail.
	_, err := st.Delete(ctx, "test", "notes", "1=1", nil, DeleteOptions{Limit: 1})
	if err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected delete beyond limit to be rejected with ErrInvalid, got %v", err)
	}
}

func TestDeleteConfirmBeyondLimit(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	res, err := st.Delete(ctx, "test", "notes", "1=1", nil, DeleteOptions{Limit: 1, Confirm: true})
	if err != nil {
		t.Fatalf("delete with confirm beyond limit: %v", err)
	}
	if res.Matched != 3 || res.Deleted != 3 {
		t.Fatalf("expected matched=3 deleted=3, got matched=%d deleted=%d", res.Matched, res.Deleted)
	}
}

func TestDeleteDefaultLimitAllowsSmall(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	res, err := st.Delete(ctx, "test", "notes", "1=1", nil, DeleteOptions{})
	if err != nil {
		t.Fatalf("small delete: %v", err)
	}
	if res.Matched != 3 || res.Deleted != 3 {
		t.Fatalf("expected matched=3 deleted=3, got matched=%d deleted=%d", res.Matched, res.Deleted)
	}
}

func TestDeleteDefaultLimitRequiresConfirm(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	if _, err := st.CreateTable(ctx, "test", "big", []schema.Field{
		{Name: "title", Type: schema.String, Fulltext: true},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	for batch := 0; batch < 2; batch++ {
		records := make([]map[string]any, 0, 600)
		for i := 0; i < 600; i++ {
			records = append(records, map[string]any{"title": fmt.Sprintf("row %d-%d", batch, i)})
		}
		if _, err := st.Insert(ctx, "test", "big", records, testEmbed); err != nil {
			t.Fatalf("insert batch %d: %v", batch, err)
		}
	}

	_, err := st.Delete(ctx, "test", "big", "1=1", nil, DeleteOptions{})
	if err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected delete beyond default limit to be rejected with ErrInvalid, got %v", err)
	}

	res, err := st.Delete(ctx, "test", "big", "1=1", nil, DeleteOptions{Confirm: true})
	if err != nil {
		t.Fatalf("delete with confirm beyond default: %v", err)
	}
	if res.Deleted != 1200 {
		t.Fatalf("expected 1200 deleted, got %d", res.Deleted)
	}
}
