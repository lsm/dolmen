package store

import (
	"context"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func scopedCalls(t *testing.T, st *Store, scope *RowScope) map[string]error {
	t.Helper()
	ctx := context.Background()
	out := map[string]error{}
	_, out["DescribeTable"] = func() (int64, error) {
		_, c, err := st.DescribeTable(ctx, "ns", "notes", scope, Incarnation{})
		return c, err
	}()
	_, out["GetRows"] = st.GetRows(ctx, "ns", "notes", []int64{1}, scope, Incarnation{})
	_, out["SearchFulltext"] = st.SearchFulltext(ctx, "ns", "notes", "alpha", "", nil, false, scope, Incarnation{}, Page{Limit: 10})
	_, out["Insert"] = st.Insert(ctx, "ns", "notes", []map[string]any{{"body": "x"}}, WriteOpts{Owner: "alice"}, Embedder{}, scope, Incarnation{})
	_, out["Update"] = st.Update(ctx, "ns", "notes", "1=1", nil, map[string]any{"body": "y"}, Embedder{}, scope, Incarnation{})
	_, out["Upsert"] = st.Upsert(ctx, "ns", "notes", "1=1", nil, map[string]any{"body": "y"}, WriteOpts{Owner: "alice"}, Embedder{}, scope, Incarnation{})
	_, out["UpsertByKey"] = st.UpsertByKey(ctx, "ns", "notes", []string{"body"}, []map[string]any{{"body": "x"}}, WriteOpts{Owner: "alice"}, Embedder{}, scope, Incarnation{})
	_, out["Delete"] = st.Delete(ctx, "ns", "notes", "1=1", nil, DeleteOpts{Confirm: true}, scope, Incarnation{})
	_, _, out["ChangesSince"] = st.ChangesSince(ctx, "ns", "notes", "", [16]byte{}, scope, Incarnation{}, Page{Limit: 10})
	_, out["PlanMigration"] = st.PlanMigration(ctx, "ns", "notes", []schema.Change{
		{Op: "add_field", Field: &schema.Field{Name: "tag", Type: schema.String}},
	}, Embedder{}, Incarnation{}, scope, Incarnation{})
	return out
}

func TestEveryScopedEntryPointEitherFiltersOrRefuses(t *testing.T) {
	st := openRowAccessStore(t)
	seedScoped(t, st)

	for name, err := range scopedCalls(t, st, &RowScope{Owner: "alice"}) {
		if err != nil {
			continue
		}
		switch name {
		case "DescribeTable", "GetRows", "SearchFulltext", "Insert":
		default:
			t.Fatalf("%s accepted a row scope without filtering by owner; an unread scope parameter is how foreign rows leak", name)
		}
	}
}

func TestScopedReadsActuallyFilter(t *testing.T) {
	st := openRowAccessStore(t)
	seedScoped(t, st)
	ctx := context.Background()
	scope := &RowScope{Owner: "alice"}

	if _, count, err := st.DescribeTable(ctx, "ns", "notes", scope, Incarnation{}); err != nil || count != 2 {
		t.Fatalf("DescribeTable count %d (err %v), want alice's 2", count, err)
	}
	rows, err := st.GetRows(ctx, "ns", "notes", []int64{1, 2, 3}, scope, Incarnation{})
	if err != nil || len(rows.Rows) != 2 {
		t.Fatalf("GetRows returned %d rows (err %v), want alice's 2", len(rows.Rows), err)
	}
	res, err := st.SearchFulltext(ctx, "ns", "notes", "alpha", "", nil, false, scope, Incarnation{}, Page{Limit: 10})
	if err != nil || len(res.Rows) != 2 {
		t.Fatalf("SearchFulltext returned %d rows (err %v), want alice's 2", len(res.Rows), err)
	}
}

func TestStaleIncarnationIsRefusedEvenUnscoped(t *testing.T) {
	st := openRowAccessStore(t)
	seedScoped(t, st)
	ctx := context.Background()

	_, inc, err := st.TableState(ctx, "ns", "notes", nil)
	if err != nil {
		t.Fatalf("table state: %v", err)
	}
	if _, err := st.Migrate(ctx, "ns", "notes", []schema.Change{
		{Op: "add_field", Field: &schema.Field{Name: "tag", Type: schema.String}},
	}, Embedder{}, Incarnation{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	for name, err := range map[string]error{
		"GetRows": func() error {
			_, e := st.GetRows(ctx, "ns", "notes", []int64{1}, nil, inc)
			return e
		}(),
		"Delete": func() error {
			_, e := st.Delete(ctx, "ns", "notes", "1=1", nil, DeleteOpts{Confirm: true}, nil, inc)
			return e
		}(),
		"Update": func() error {
			_, e := st.Update(ctx, "ns", "notes", "1=1", nil, map[string]any{"body": "y"}, Embedder{}, nil, inc)
			return e
		}(),
		"Insert": func() error {
			_, e := st.Insert(ctx, "ns", "notes", []map[string]any{{"body": "x"}}, WriteOpts{Owner: "alice"}, Embedder{}, nil, inc)
			return e
		}(),
	} {
		if err == nil {
			t.Fatalf("%s executed against a stale incarnation; a request resolved before a migration must not run afterwards", name)
		}
	}
}
