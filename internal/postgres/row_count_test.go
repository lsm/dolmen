package postgres

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func seedPostgresCountedTable(t *testing.T, s *Store, opts store.TableOpts, owner string, rows int) string {
	t.Helper()
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body", Type: schema.Text}}, opts, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	records := make([]map[string]any, rows)
	for i := range records {
		records[i] = map[string]any{"body": "row"}
	}
	if _, err := s.Insert(ctx, "app", "notes", records, store.WriteOpts{Owner: owner}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	var relation string
	if err := s.read(ctx, "app", func(tx pgx.Tx, n namespace) error {
		state, err := s.loadTable(ctx, tx, n, "notes")
		relation = ident(n.physical, state.physical)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return relation
}

func TestPostgresDescribeTableReadsTheMaintainedCountInsteadOfScanning(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	seedPostgresCountedTable(t, s, store.TableOpts{}, "", 3)
	tag, err := s.pool.Exec(ctx, "UPDATE "+s.relation("row_counts")+" SET n = 41 WHERE NOT scoped")
	if err != nil {
		t.Fatalf("the catalog keeps no maintained row count: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("planted %d total counts, want exactly the one table's", tag.RowsAffected())
	}
	if _, count, err := s.DescribeTable(ctx, "app", "notes", nil, store.Incarnation{}); err != nil || count != 41 {
		t.Fatalf("describe_table reported %d (%v); it must read the maintained count (planted as 41), not scan the table", count, err)
	}
}

func TestPostgresCatalogUpgradeBackfillsRowCounts(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	relation := seedPostgresCountedTable(t, s, store.TableOpts{RowAccess: schema.RowAccessOwn}, "alice", 4)
	for _, stmt := range []string{
		"DROP TRIGGER " + rowCountInsertTrigger + " ON " + relation,
		"DROP TRIGGER " + rowCountDeleteTrigger + " ON " + relation,
		"DROP TABLE " + s.relation("row_counts"),
		"DROP FUNCTION " + s.relation("count_rows") + "()",
		"UPDATE " + s.relation("version") + " SET version = 6",
	} {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("shape the catalog as version 6 (%s): %v", stmt, err)
		}
	}

	if err := s.bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap over a catalog that kept no row counts: %v", err)
	}
	var version int
	if err := s.pool.QueryRow(ctx, "SELECT version FROM "+s.relation("version")).Scan(&version); err != nil || version != catalogVersion {
		t.Fatalf("catalog version %d after bootstrap, want %d: %v", version, catalogVersion, err)
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "bob's"}}, store.WriteOpts{Owner: "bob"}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		scope *store.RowScope
		want  int64
	}{{nil, 5}, {&store.RowScope{Owner: "alice"}, 4}, {&store.RowScope{Owner: "bob"}, 1}, {&store.RowScope{Owner: "carol"}, 0}} {
		if _, count, err := s.DescribeTable(ctx, "app", "notes", c.scope, store.Incarnation{}); err != nil || count != c.want {
			t.Fatalf("after the upgrade, describe_table under scope %+v reported %d (%v), want %d", c.scope, count, err, c.want)
		}
	}
}

func TestPostgresDescribeTableDistrustsCountsAnOlderProcessLeft(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	relation := seedPostgresCountedTable(t, s, store.TableOpts{RowAccess: schema.RowAccessOwn}, "alice", 3)
	for _, stmt := range []string{
		"DROP TRIGGER " + rowCountInsertTrigger + " ON " + relation,
		"DROP TRIGGER " + rowCountDeleteTrigger + " ON " + relation,
		"UPDATE " + s.relation("row_counts") + " SET tracks_owners = false WHERE NOT scoped",
		"DELETE FROM " + s.relation("row_counts") + " WHERE scoped",
	} {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("shape counts a version 6 process left (%s): %v", stmt, err)
		}
	}
	if _, err := s.pool.Exec(ctx, "INSERT INTO "+relation+" (body, owner) VALUES ('b1', 'bob'), ('b2', 'bob')"); err != nil {
		t.Fatalf("a write the counts never saw: %v", err)
	}
	if _, count, err := s.DescribeTable(ctx, "app", "notes", &store.RowScope{Owner: "bob"}, store.Incarnation{}); err != nil || count != 2 {
		t.Fatalf("counts that never tracked owners reported %d (%v) for bob, want the scanned 2", count, err)
	}

	if _, err := s.pool.Exec(ctx, "UPDATE "+s.relation("tables")+" SET drop_generation = drop_generation + 1 WHERE namespace = 'app' AND name = 'notes'"); err != nil {
		t.Fatalf("shape a table an older process recreated: %v", err)
	}
	if _, count, err := s.DescribeTable(ctx, "app", "notes", nil, store.Incarnation{}); err != nil || count != 5 {
		t.Fatalf("counts kept for an earlier incarnation reported %d (%v), want the scanned 5", count, err)
	}
}
