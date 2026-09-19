package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestPhysicalColumnsResolveShortenedNameCollision(t *testing.T) {
	long := strings.Repeat("a", 64)
	colliding := physicalCandidate(long, 0)
	columns, err := physicalColumns([]schema.Field{{Name: long}, {Name: colliding}, {Name: strings.Repeat("a", 63) + "b"}})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for logical, physical := range columns {
		if seen[physical] || len(physical) > 63 {
			t.Fatalf("invalid mapping %q -> %q", logical, physical)
		}
		seen[physical] = true
	}
	if columns[colliding] != colliding {
		t.Fatal("ordinary short identifier was renamed")
	}
}

func TestPostgresTableSchemaAndNativeTypes(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	other := openTest(t, cfg)
	ctx := context.Background()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("a", 64)
	fields := []schema.Field{
		{Name: long, Type: schema.String, Required: true, Fulltext: true},
		{Name: physicalCandidate(long, 0), Type: schema.Number, Default: json.Number("9007199254740993")},
		{Name: "active", Type: schema.Boolean}, {Name: "body", Type: schema.Text, Vectorize: true},
		{Name: "metadata", Type: schema.JSON}, {Name: "observed", Type: schema.Timestamp},
		{Name: "embedding", Type: schema.Vector, Dim: 3},
	}
	sc, err := s.CreateTable(ctx, "app", long, fields, store.TableOpts{}, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if sc.Version != 1 {
		t.Fatalf("version %d", sc.Version)
	}
	described, inc, err := other.TableState(ctx, "app", long, nil)
	if err != nil {
		t.Fatal(err)
	}
	if described.Fields[0].Name != long || described.Fields[1].Default != json.Number("9007199254740993") {
		t.Fatalf("schema fidelity: %+v", described)
	}
	if inc.Table != long || inc.Version != 1 || inc.DropGen != 0 || inc.NsGen == [16]byte{} {
		t.Fatalf("incarnation %+v", inc)
	}
	var state tableState
	err = s.write(ctx, "app", inc.NsGen, func(tx pgx.Tx, n namespace) error {
		var err error
		state, err = s.loadTable(ctx, tx, n, long)
		if err != nil {
			return err
		}
		stmt := "INSERT INTO " + ident(n.physical, state.physical) + "(" + ident(state.columns[long]) + "," + ident(state.columns[fields[1].Name]) + "," + ident("active") + ") VALUES($1,$2,$3)"
		_, err = tx.Exec(ctx, stmt, "sample", "9007199254740993", true)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	_, count, err := other.DescribeTable(ctx, "app", long, nil, inc)
	if err != nil || count != 1 {
		t.Fatalf("count %d: %v", count, err)
	}
	err = s.read(ctx, "app", func(tx pgx.Tx, n namespace) error {
		var number, created string
		var active bool
		err := tx.QueryRow(ctx, "SELECT "+ident(state.columns[fields[1].Name])+"::text,created_at,active FROM "+ident(n.physical, state.physical)).Scan(&number, &created, &active)
		if err == nil && (number != "9007199254740993" || !active || len(created) != 24) {
			t.Errorf("native stored values: %q %q %t", number, created, active)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.DropTable(ctx, "app", long, inc); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.TableState(ctx, "app", long, nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("dropped table: %v", err)
	}
	if _, err := s.CreateTable(ctx, "app", long, fields, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	_, next, err := s.TableState(ctx, "app", long, nil)
	if err != nil || next.DropGen != 1 {
		t.Fatalf("new incarnation %+v %v", next, err)
	}
	if err := other.DropTable(ctx, "app", long, inc); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale incarnation dropped successor: %v", err)
	}
	names, err := s.ListTables(ctx, "app", nil)
	if err != nil || len(names) != 1 || names[0] != long {
		t.Fatalf("logical names: %v %v", names, err)
	}
}

func TestPostgresTableDDLAndRegistryAreAtomic(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	other := openTest(t, cfg)
	ctx := context.Background()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	fields := []schema.Field{{Name: "body", Type: schema.Text}}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, engine := range []*Store{s, other} {
		wg.Go(func() {
			<-start
			_, err := engine.CreateTable(ctx, "app", "notes", fields, store.TableOpts{}, [16]byte{})
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	success, duplicate := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, store.ErrInvalid) {
			duplicate++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || duplicate != 1 {
		t.Fatalf("concurrent create: %d success %d duplicate", success, duplicate)
	}
	_, inc, _ := s.TableState(ctx, "app", "notes", nil)
	bad := inc
	bad.Version++
	if err := s.DropTable(ctx, "app", "notes", bad); err == nil {
		t.Fatal("version mismatch accepted")
	}
	if err := s.DropNamespace(ctx, "app", inc.NsGen); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", fields, store.TableOpts{}, inc.NsGen); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale namespace table creation: %v", err)
	}
	names, err := s.ListTables(ctx, "app", nil)
	if err != nil || len(names) != 0 {
		t.Fatalf("old namespace registry survived: %v %v", names, err)
	}
}

func TestPostgresPhysicalTableAvoidsIndexNameCollision(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	ctx := context.Background()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"notes", "notes_pkey", "notes_id_seq", strings.Repeat("z", 64), physicalCandidate(strings.Repeat("z", 64), 0)} {
		if _, err := s.CreateTable(ctx, "app", name, []schema.Field{{Name: "body"}}, store.TableOpts{}, [16]byte{}); err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
	}
	names, err := s.ListTables(ctx, "app", nil)
	if err != nil || len(names) != 5 {
		t.Fatalf("tables: %v %v", names, err)
	}
}

func TestPostgresCatalogUpgradeFromNamespaceFoundation(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	ctx := context.Background()
	if err := s.CreateNamespace(ctx, "existing", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	gen, _ := s.NamespaceState(ctx, "existing", nil)
	if _, err := s.pool.Exec(ctx, "DROP TABLE "+s.relation("tables")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, "UPDATE "+s.relation("version")+" SET version=1"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	upgraded := openTest(t, cfg)
	again, err := upgraded.NamespaceState(ctx, "existing", nil)
	if err != nil || again != gen {
		t.Fatalf("upgrade changed namespace: %v", err)
	}
	if _, err := upgraded.CreateTable(ctx, "existing", "notes", []schema.Field{{Name: "body"}}, store.TableOpts{}, gen); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresFailedRegistryWriteRollsBackDDL(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	ctx := context.Background()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	_, err := s.pool.Exec(ctx, "CREATE FUNCTION "+ident(s.catalog, "reject_table")+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected registry failure'; END $$`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(ctx, "CREATE TRIGGER reject_table BEFORE INSERT ON "+s.relation("tables")+" FOR EACH ROW EXECUTE FUNCTION "+ident(s.catalog, "reject_table")+"()")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "rejected", []schema.Field{{Name: "body"}}, store.TableOpts{}, [16]byte{}); err == nil {
		t.Fatal("injected failure was ignored")
	}
	err = s.read(ctx, "app", func(tx pgx.Tx, n namespace) error {
		var count int
		err := tx.QueryRow(ctx, "SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1", n.physical).Scan(&count)
		if err == nil && count != 0 {
			t.Errorf("failed registry write left %d physical relations", count)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	names, err := s.ListTables(ctx, "app", nil)
	if err != nil || len(names) != 0 {
		t.Fatalf("failed registry write left metadata: %v %v", names, err)
	}
}
