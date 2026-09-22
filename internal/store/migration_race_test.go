package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
)

func resolveScopeFor(t *testing.T, st *Store, owner string) (*RowScope, Incarnation) {
	t.Helper()
	sc, inc, err := st.TableState(context.Background(), "ns", "notes", nil)
	if err != nil {
		t.Fatalf("table state: %v", err)
	}
	if sc.RowAccess != schema.RowAccessOwn {
		t.Fatalf("the fixture is not a row_access table")
	}
	return &RowScope{Owner: owner}, inc
}

func seedRacingTable(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "ns", "notes", []schema.Field{
		{Name: "body", Type: schema.Text, Fulltext: true},
	}, TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 0; i < 8; i++ {
		owner := "bob"
		if i%2 == 0 {
			owner = "alice"
		}
		if _, err := st.Insert(ctx, "ns", "notes", []map[string]any{{"body": "note from " + owner}},
			WriteOpts{Owner: owner}, Embedder{}, nil, Incarnation{}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
}

func TestAMigrationRacingAScopedReadConvergesOnConflict(t *testing.T) {
	st := openRowAccessStore(t)
	ctx := context.Background()
	seedRacingTable(t, st)

	var wg sync.WaitGroup
	readers := 6
	results := make(chan error, readers)
	start := make(chan struct{})
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			scope, inc := resolveScopeFor(t, st, "alice")
			<-start
			rows, err := st.GetRows(ctx, "ns", "notes", []int64{1, 2, 3, 4, 5, 6, 7, 8}, scope, inc)
			if err != nil {
				results <- err
				return
			}
			for _, row := range rows.Rows {
				if body, _ := row["body"].(string); body != "note from alice" {
					results <- fmt.Errorf("a scoped read returned a row owned by someone else: %v", row)
					return
				}
			}
			results <- nil
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		if _, err := st.Migrate(ctx, "ns", "notes", []schema.Change{
			{Op: schema.OpAddField, Field: &schema.Field{Name: "tag", Type: schema.String}},
		}, Embedder{}, Incarnation{}); err != nil {
			t.Errorf("migration: %v", err)
		}
	}()

	close(start)
	wg.Wait()
	close(results)

	conflicts := 0
	for err := range results {
		switch {
		case err == nil:
		case errors.Is(err, derr.ErrConflict):
			conflicts++
		default:
			t.Fatalf("a scoped read racing a migration answered with neither its own rows nor a conflict: %v", err)
		}
	}

	scope, inc := resolveScopeFor(t, st, "alice")
	rows, err := st.GetRows(ctx, "ns", "notes", []int64{1, 2, 3, 4, 5, 6, 7, 8}, scope, inc)
	if err != nil {
		t.Fatalf("re-resolving after the migration must succeed, which is what makes the conflict a retry: %v (%d conflicted)", err, conflicts)
	}
	if len(rows.Rows) != 4 {
		t.Fatalf("alice owns four of the eight rows: %d", len(rows.Rows))
	}
}

func TestAMigrationRacingAScopedDeleteNeverTouchesAForeignRow(t *testing.T) {
	st := openRowAccessStore(t)
	ctx := context.Background()
	seedRacingTable(t, st)

	var wg sync.WaitGroup
	start := make(chan struct{})
	deleted := make(chan int64, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			scope, inc := resolveScopeFor(t, st, "alice")
			<-start
			res, err := st.Delete(ctx, "ns", "notes", "1=1", nil, DeleteOpts{Confirm: true}, scope, inc)
			if err != nil {
				if !errors.Is(err, derr.ErrConflict) {
					t.Errorf("a scoped delete racing a migration answered with neither a count nor a conflict: %v", err)
				}
				return
			}
			deleted <- res.Deleted
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		if _, err := st.Migrate(ctx, "ns", "notes", []schema.Change{
			{Op: schema.OpAddField, Field: &schema.Field{Name: "tag", Type: schema.String}},
		}, Embedder{}, Incarnation{}); err != nil {
			t.Errorf("migration: %v", err)
		}
	}()

	close(start)
	wg.Wait()
	close(deleted)

	total := int64(0)
	for n := range deleted {
		total += n
	}
	if total > 4 {
		t.Fatalf("scoped deletes removed %d rows, and alice owns only four — a migration mid-flight widened one of them", total)
	}

	rows, err := st.GetRows(ctx, "ns", "notes", []int64{1, 2, 3, 4, 5, 6, 7, 8}, &RowScope{Owner: "bob"}, Incarnation{})
	if err != nil {
		t.Fatalf("read bob's rows: %v", err)
	}
	if len(rows.Rows) != 4 {
		t.Fatalf("bob owns four rows and no scoped delete of alice's may touch them: %d left", len(rows.Rows))
	}
}

func TestAStaleScopeConflictsAndItsRetrySucceeds(t *testing.T) {
	st := openRowAccessStore(t)
	ctx := context.Background()
	seedRacingTable(t, st)

	scope, stale := resolveScopeFor(t, st, "alice")
	if _, err := st.Migrate(ctx, "ns", "notes", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "tag", Type: schema.String}},
	}, Embedder{}, Incarnation{}); err != nil {
		t.Fatalf("migration: %v", err)
	}

	_, err := st.GetRows(ctx, "ns", "notes", []int64{1, 2, 3, 4}, scope, stale)
	if !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("a scope resolved before the migration must conflict rather than read against a table it never saw: %v", err)
	}

	fresh, freshInc := resolveScopeFor(t, st, "alice")
	rows, err := st.GetRows(ctx, "ns", "notes", []int64{1, 2, 3, 4, 5, 6, 7, 8}, fresh, freshInc)
	if err != nil {
		t.Fatalf("re-resolving is the whole remedy the conflict names, so it must succeed: %v", err)
	}
	if len(rows.Rows) != 4 {
		t.Fatalf("alice still owns four rows after the migration: %d", len(rows.Rows))
	}
}
