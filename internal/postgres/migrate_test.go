package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func migrateTrue() *bool { v := true; return &v }

func migrateEnum(v []string) *[]string { return &v }

func fixedEmbedder(calls *int) store.Embedder {
	return store.Embedder{Identity: "test", Embed: func(_ context.Context, texts []string) ([][]float32, error) {
		if calls != nil {
			*calls += len(texts)
		}
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1, 0, float32(len(texts[i]))}
		}
		return out, nil
	}}
}

func TestPostgresMigrateKeepsPhysicalNameMapping(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("l", 64)
	longer := strings.Repeat("m", 64)
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: long}, {Name: "keep"}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{long: "first", "keep": "k"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	before, err := s.columnMapping(ctx, "app", "notes")
	if err != nil {
		t.Fatal(err)
	}
	if before[long] == long || len(before[long]) > 63 {
		t.Fatalf("long field not hashed: %+v", before)
	}
	if _, err := s.Migrate(ctx, "app", "notes", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: longer}, Default: "backfilled"},
	}, store.Embedder{}, store.Incarnation{Version: 1}); err != nil {
		t.Fatal(err)
	}
	after, err := s.columnMapping(ctx, "app", "notes")
	if err != nil {
		t.Fatal(err)
	}
	if after[long] != before[long] {
		t.Fatalf("existing mapping moved: %q -> %q", before[long], after[long])
	}
	if after[longer] == "" || len(after[longer]) > 63 || after[longer] == after[long] {
		t.Fatalf("new long field mapping: %+v", after)
	}
	rows, err := s.GetRows(ctx, "app", "notes", []int64{1}, nil, store.Incarnation{})
	if err != nil || len(rows.Rows) != 1 {
		t.Fatalf("rows: %+v %v", rows, err)
	}
	if rows.Rows[0][long] != "first" || rows.Rows[0][longer] != "backfilled" {
		t.Fatalf("values after migrate: %+v", rows.Rows[0])
	}

	renamed := strings.Repeat("r", 64)
	if _, err := s.Migrate(ctx, "app", "notes", []schema.Change{
		{Op: schema.OpRenameField, From: long, To: renamed},
	}, store.Embedder{}, store.Incarnation{Version: 2}); err != nil {
		t.Fatal(err)
	}
	final, err := s.columnMapping(ctx, "app", "notes")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := final[long]; ok {
		t.Fatalf("old logical name kept: %+v", final)
	}
	if len(final[renamed]) > 63 || final[renamed] == "" {
		t.Fatalf("renamed mapping: %+v", final)
	}
	rows, err = s.GetRows(ctx, "app", "notes", []int64{1}, nil, store.Incarnation{})
	if err != nil || rows.Rows[0][renamed] != "first" {
		t.Fatalf("rename preserved data: %+v %v", rows.Rows, err)
	}
}

func (s *Store) columnMapping(ctx context.Context, ns, table string) (map[string]string, error) {
	var out map[string]string
	err := s.read(ctx, ns, func(tx pgx.Tx, n namespace) error {
		state, err := s.loadTable(ctx, tx, n, table)
		if err != nil {
			return err
		}
		out = state.columns
		return nil
	})
	return out, err
}

func TestPostgresMigrateRegrantsQueryRole(t *testing.T) {
	cfg := testConfig(t)
	if cfg.QueryRole == "" {
		t.Skip("set DOLMEN_TEST_PG_QUERY_ROLE for caller SQL grant coverage")
	}
	s := openTest(t, cfg)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body"}, {Name: "gone"}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "b", "gone": "g"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Query(ctx, "app", "SELECT body FROM notes", nil, [16]byte{}, store.Page{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx, "app", "notes", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "added"}, Default: "a"},
		{Op: schema.OpDropField, Name: "gone"},
	}, store.Embedder{}, store.Incarnation{Version: 1}); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "app", "SELECT added FROM notes", nil, [16]byte{}, store.Page{})
	if err != nil {
		t.Fatalf("new column not granted to the query role: %v", err)
	}
	if len(result.Rows) != 1 || result.Rows[0]["added"] != "a" {
		t.Fatalf("added column: %+v", result.Rows)
	}
	if _, err := s.Query(ctx, "app", "SELECT gone FROM notes", nil, [16]byte{}, store.Page{}); err == nil {
		t.Fatal("dropped column still selectable")
	}
}

func TestPostgresMigrateEmbedsOutsideWriteTransaction(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"notes", "other"} {
		if _, err := s.CreateTable(ctx, "app", table, []schema.Field{{Name: "body"}}, store.TableOpts{}, [16]byte{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "one"}, {"body": "two"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	concurrent := 0
	emb := store.Embedder{Identity: "test", Embed: func(context.Context, []string) ([][]float32, error) {
		inner, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if _, err := s.Insert(inner, "app", "other", []map[string]any{{"body": "written during backfill"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
			return nil, err
		}
		concurrent++
		return [][]float32{{1, 0, 0}, {0, 1, 0}}, nil
	}}
	if _, err := s.Migrate(ctx, "app", "notes", []schema.Change{
		{Op: schema.OpSetVectorize, Name: "body", Value: migrateTrue()},
	}, emb, store.Incarnation{Version: 1}); err != nil {
		t.Fatalf("backfill held the namespace write lock: %v", err)
	}
	if concurrent == 0 {
		t.Fatal("embedding provider was never called")
	}
	rows, err := s.GetRows(ctx, "app", "other", []int64{1}, nil, store.Incarnation{})
	if err != nil || len(rows.Rows) != 1 {
		t.Fatalf("concurrent write lost: %+v %v", rows, err)
	}
	var embedded int
	if err := s.read(ctx, "app", func(tx pgx.Tx, n namespace) error {
		state, err := s.loadTable(ctx, tx, n, "notes")
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, "SELECT count(*) FROM "+ident(n.physical, state.physical)+` WHERE "_embedding" IS NOT NULL`).Scan(&embedded)
	}); err != nil {
		t.Fatal(err)
	}
	if embedded != 2 {
		t.Fatalf("backfilled %d rows, want 2", embedded)
	}
}

func TestPostgresMigrateRollsBackFailedStep(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body"}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "one"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	failing := store.Embedder{Identity: "test", Embed: func(context.Context, []string) ([][]float32, error) {
		return nil, errors.New("provider offline")
	}}
	if _, err := s.Migrate(ctx, "app", "notes", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "extra"}, Default: "x"},
		{Op: schema.OpSetVectorize, Name: "body", Value: migrateTrue()},
	}, failing, store.Incarnation{Version: 1}); err == nil {
		t.Fatal("failing provider accepted")
	}
	sc, _, err := s.TableState(ctx, "app", "notes", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Version != 1 || sc.Field("extra") != nil {
		t.Fatalf("failed migration left schema changes: %+v", sc)
	}
	if err := s.read(ctx, "app", func(tx pgx.Tx, n namespace) error {
		state, err := s.loadTable(ctx, tx, n, "notes")
		if err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid=a.attrelid JOIN pg_catalog.pg_namespace ns ON ns.oid=c.relnamespace WHERE ns.nspname=$1 AND c.relname=$2 AND a.attname='extra' AND NOT a.attisdropped)`, n.physical, state.physical).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return errors.New("rolled-back column still present")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	history, err := s.ListMigrations(ctx, "app", "notes", store.Incarnation{})
	if err != nil || len(history) != 0 {
		t.Fatalf("failed migration recorded history: %+v %v", history, err)
	}
}

func TestPostgresMigrateConcurrentVersionCAS(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body"}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make([]error, 2)
	names := []string{"alpha", "beta"}
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = s.Migrate(ctx, "app", "notes", []schema.Change{
				{Op: schema.OpAddField, Field: &schema.Field{Name: names[i]}},
			}, store.Embedder{}, store.Incarnation{Version: 1})
		}(i)
	}
	wg.Wait()
	winners := 0
	for _, err := range results {
		if err == nil {
			winners++
			continue
		}
		var conflict *store.VersionConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("loser error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("expected exactly one migration to win, got %d (%v)", winners, results)
	}
	sc, _, err := s.TableState(ctx, "app", "notes", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Version != 2 {
		t.Fatalf("version after race: %+v", sc)
	}
	history, err := s.ListMigrations(ctx, "app", "notes", store.Incarnation{})
	if err != nil || len(history) != 1 {
		t.Fatalf("history after race: %+v %v", history, err)
	}
}

func TestPostgresMigrateReembedsOnVectorizeCycle(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body", Vectorize: true}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	emb := fixedEmbedder(&calls)
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "one"}, {"body": "two"}}, store.WriteOpts{}, emb, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx, "app", "notes", []schema.Change{
		{Op: schema.OpSetVectorize, Name: "body", Value: new(bool)},
	}, emb, store.Incarnation{Version: 1}); err != nil {
		t.Fatal(err)
	}
	var remaining int
	count := func() int {
		if err := s.read(ctx, "app", func(tx pgx.Tx, n namespace) error {
			state, err := s.loadTable(ctx, tx, n, "notes")
			if err != nil {
				return err
			}
			return tx.QueryRow(ctx, "SELECT count(*) FROM "+ident(n.physical, state.physical)+` WHERE "_embedding" IS NOT NULL`).Scan(&remaining)
		}); err != nil {
			t.Fatal(err)
		}
		return remaining
	}
	if got := count(); got != 0 {
		t.Fatalf("disabling vectorize left %d embeddings", got)
	}
	before := calls
	sc, err := s.Migrate(ctx, "app", "notes", []schema.Change{
		{Op: schema.OpSetVectorize, Name: "body", Value: migrateTrue()},
	}, emb, store.Incarnation{Version: 2})
	if err != nil {
		t.Fatal(err)
	}
	if calls-before != 2 {
		t.Fatalf("re-embedded %d texts, want 2", calls-before)
	}
	if sc.EmbedDim != 3 || sc.EmbedSpace != "test" {
		t.Fatalf("embedding metadata: %+v", sc)
	}
	if got := count(); got != 2 {
		t.Fatalf("re-enabling vectorize embedded %d rows, want 2", got)
	}
}

func TestPostgresMigrateAddsVectorizedFieldWithDefault(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "code"}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"code": "a"}, {"code": "b"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	emb := fixedEmbedder(&calls)
	plan, err := s.PlanMigration(ctx, "app", "notes", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "summary", Vectorize: true}, Default: "placeholder"},
	}, emb, store.Incarnation{Version: 1}, nil, store.Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.EmbedRows != 2 || plan.BackfillRows != 2 || plan.ClearsEmbeddings {
		t.Fatalf("plan: %+v", plan)
	}
	if calls != 0 {
		t.Fatalf("dry run embedded %d texts", calls)
	}
	if _, err := s.Migrate(ctx, "app", "notes", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "summary", Vectorize: true}, Default: "placeholder"},
	}, emb, store.Incarnation{Version: 1}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("constant backfill embedded %d texts, want 1", calls)
	}
	var embedded int
	if err := s.read(ctx, "app", func(tx pgx.Tx, n namespace) error {
		state, err := s.loadTable(ctx, tx, n, "notes")
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, "SELECT count(*) FROM "+ident(n.physical, state.physical)+` WHERE "_embedding" IS NOT NULL`).Scan(&embedded)
	}); err != nil {
		t.Fatal(err)
	}
	if embedded != 2 {
		t.Fatalf("embedded %d rows, want 2", embedded)
	}
}

func TestPostgresMigrateRejectsEnumValueRacingTheWriteLock(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "state"}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"state": "open"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	racing := store.Embedder{Identity: "test", Embed: func(_ context.Context, texts []string) ([][]float32, error) {
		if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"state": "archived"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
			return nil, err
		}
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1, 0, 0}
		}
		return out, nil
	}}
	_, err := s.Migrate(ctx, "app", "notes", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "body", Vectorize: true}, Default: "seed"},
		{Op: schema.OpSetEnum, Name: "state", Enum: migrateEnum([]string{"open"})},
	}, racing, store.Incarnation{Version: 1})
	if !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("value written between plan and apply was accepted: %v", err)
	}
	sc, _, err := s.TableState(ctx, "app", "notes", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Version != 1 || sc.Field("body") != nil || sc.Field("state").Enum != nil {
		t.Fatalf("rolled-back migration left changes: %+v", sc)
	}
	rows, err := s.GetRows(ctx, "app", "notes", []int64{1, 2}, nil, store.Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Rows) != 2 {
		t.Fatalf("racing insert lost: %+v", rows.Rows)
	}
}

func TestPostgresMigrateBackfillsInPagedBatches(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body"}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	const rows = embedBackfillPage*2 + 7
	records := make([]map[string]any, rows)
	for i := range records {
		records[i] = map[string]any{"body": fmt.Sprintf("row %d", i)}
	}
	if _, err := s.Insert(ctx, "app", "notes", records, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	batches := []int{}
	emb := store.Embedder{Identity: "test", Embed: func(_ context.Context, texts []string) ([][]float32, error) {
		mu.Lock()
		batches = append(batches, len(texts))
		mu.Unlock()
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1, 0, float32(len(texts[i]))}
		}
		return out, nil
	}}
	if _, err := s.Migrate(ctx, "app", "notes", []schema.Change{
		{Op: schema.OpSetVectorize, Name: "body", Value: migrateTrue()},
	}, emb, store.Incarnation{Version: 1}); err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, n := range batches {
		if n > embedBackfillPage {
			t.Fatalf("batch of %d exceeds the %d-row page: %+v", n, embedBackfillPage, batches)
		}
		total += n
	}
	if total != rows {
		t.Fatalf("embedded %d texts, want %d", total, rows)
	}
	if len(batches) < 3 {
		t.Fatalf("expected the backfill to page, got batches %+v", batches)
	}
	var embedded int
	if err := s.read(ctx, "app", func(tx pgx.Tx, n namespace) error {
		state, err := s.loadTable(ctx, tx, n, "notes")
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, "SELECT count(*) FROM "+ident(n.physical, state.physical)+` WHERE "_embedding" IS NOT NULL`).Scan(&embedded)
	}); err != nil {
		t.Fatal(err)
	}
	if embedded != rows {
		t.Fatalf("backfilled %d rows, want %d", embedded, rows)
	}
}

func TestPostgresPlanCountsOnlyVisibleRows(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body"}},
		store.TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"alice", "bob", "bob", "bob"} {
		if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "from " + who}},
			store.WriteOpts{Owner: who}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
			t.Fatal(err)
		}
	}
	backfill := func(scope *store.RowScope) int64 {
		t.Helper()
		plan, err := s.PlanMigration(ctx, "app", "notes", []schema.Change{
			{Op: schema.OpAddField, Field: &schema.Field{Name: "tag", Type: schema.String}, Default: "x"},
		}, store.Embedder{}, store.Incarnation{}, scope, store.Incarnation{})
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		return plan.BackfillRows
	}
	if got := backfill(nil); got != 4 {
		t.Fatalf("a table-wide reader sees every row: backfill_rows %d, want 4", got)
	}
	if got := backfill(&store.RowScope{Owner: "alice"}); got != 1 {
		t.Fatalf("alice wrote one of the four rows, so the plan may disclose only that: backfill_rows %d, want 1", got)
	}
	if got := backfill(&store.RowScope{Empty: true}); got != 0 {
		t.Fatalf("a schema-only holder sees no row at all: backfill_rows %d, want 0", got)
	}
}
