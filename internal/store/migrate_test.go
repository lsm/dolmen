package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func TestMigrate(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	sc, err := st.Migrate(ctx, "test", "notes", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "priority", Type: schema.Number}},
		{Op: schema.OpRenameField, From: "title", To: "heading"},
	}, testEmbed)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if sc.Version != 2 {
		t.Fatalf("expected version 2, got %d", sc.Version)
	}

	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"heading": "fourth note", "body": "new row after migration", "priority": 7, "emb": []any{0, 0, 1.0, 0}},
	}, testEmbed); err != nil {
		t.Fatalf("insert after migrate: %v", err)
	}

	rows, _, err := st.SearchFulltext(ctx, "test", "notes", "dolmen", 10)
	if err != nil {
		t.Fatalf("fts after rename: %v", err)
	}
	if len(rows) != 1 || rows[0]["heading"] != "first note" {
		t.Fatalf("fts not rebuilt after rename: %v", rows)
	}

	rows, _, err = st.Query(ctx, "test", "SELECT priority FROM notes WHERE heading = 'fourth note'", nil)
	if err != nil {
		t.Fatalf("query new column: %v", err)
	}
	if got := rows[0]["priority"].(int64); got != 7 {
		t.Fatalf("expected priority 7, got %v", got)
	}
}

func TestMigrateVectorizeBackfill(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	if _, err := st.CreateTable(ctx, "test", "plain", []schema.Field{
		{Name: "note", Type: schema.String, Fulltext: true},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "plain", []map[string]any{
		{"note": "hello world"},
		{"note": "goodbye world"},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if _, err := st.Migrate(ctx, "test", "plain", []schema.Change{
		{Op: schema.OpSetVectorize, Name: "note", Value: true},
	}, testEmbed); err != nil {
		t.Fatalf("migrate vectorize: %v", err)
	}

	qv, _ := fakeEmbed(ctx, []string{"hello world"})
	rows, _, err := st.SearchVector(ctx, "test", "plain", "", qv[0], "fake-space", 1)
	if err != nil {
		t.Fatalf("vector search after backfill: %v", err)
	}
	if len(rows) != 1 || rows[0]["note"] != "hello world" {
		t.Fatalf("unexpected backfill results: %v", rows)
	}
}

func TestDropFieldAndVersioning(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	sc, err := st.Migrate(ctx, "test", "notes", []schema.Change{
		{Op: schema.OpDropField, Name: "tags"},
	}, testEmbed)
	if err != nil {
		t.Fatalf("drop: %v", err)
	}
	if sc.Version != 2 || sc.Field("tags") != nil {
		t.Fatalf("drop did not apply: %+v", sc)
	}

	names, err := st.ListTables(ctx, "test")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(names) != 1 || names[0] != "notes" {
		t.Fatalf("unexpected tables: %v", names)
	}

	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"title": "x", "tags": []any{"nope"}},
	}, testEmbed); err == nil {
		t.Fatal("expected dropped field to be rejected on insert")
	}
}

func TestMigrateVectorizeSwitch(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	if _, err := st.CreateTable(ctx, "test", "switch", []schema.Field{
		{Name: "a", Type: schema.String, Fulltext: true, Vectorize: true},
		{Name: "b", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "switch", []map[string]any{
		{"a": "alpha content here", "b": "beta content here"},
		{"a": "gamma content here", "b": "delta content here"},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if _, err := st.Migrate(ctx, "test", "switch", []schema.Change{
		{Op: schema.OpSetVectorize, Name: "a", Value: false},
		{Op: schema.OpSetVectorize, Name: "b", Value: true},
	}, testEmbed); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	qv, _ := fakeEmbed(ctx, []string{"delta content here"})
	rows, _, err := st.SearchVector(ctx, "test", "switch", "", qv[0], "fake-space", 2)
	if err != nil {
		t.Fatalf("vector search after switch: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no rows found after vectorize switch")
	}
	if rows[0]["b"] != "delta content here" {
		t.Fatalf("stale embeddings from field a: top hit was %v", rows[0])
	}
	if score := rows[0]["_score"].(float64); score < 0.99 {
		t.Fatalf("expected cosine ~1 for exact text, got %f", score)
	}
}

func TestDropAndReAddVectorizeField(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "recyc", []schema.Field{
		{Name: "a", Type: schema.String, Vectorize: true},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "recyc", []map[string]any{
		{"a": "old one"}, {"a": "old two"},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if _, err := st.Migrate(ctx, "test", "recyc", []schema.Change{
		{Op: schema.OpDropField, Name: "a"},
		{Op: schema.OpAddField, Field: &schema.Field{Name: "a", Type: schema.String, Vectorize: true}},
	}, testEmbed); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if _, err := st.Insert(ctx, "test", "recyc", []map[string]any{
		{"a": "fresh row"},
	}, testEmbed); err != nil {
		t.Fatalf("insert after re-add: %v", err)
	}

	qv, _ := fakeEmbed(ctx, []string{"fresh row"})
	rows, _, err := st.SearchVector(ctx, "test", "recyc", "", qv[0], "fake-space", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(rows) != 1 || rows[0]["a"] != "fresh row" {
		t.Fatalf("stale embeddings survived drop+re-add: %v", rows)
	}
}

func TestEmbedModelMismatchGuard(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "mm", []schema.Field{
		{Name: "s", Type: schema.String, Vectorize: true},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "mm", []map[string]any{{"s": "hello"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}

	other := Embedder{Embed: fakeEmbed, Identity: "other-space"}
	if _, err := st.Insert(ctx, "test", "mm", []map[string]any{{"s": "more"}}, other); err == nil {
		t.Fatal("expected insert with changed model to be rejected")
	}

	qv, _ := fakeEmbed(ctx, []string{"hello"})
	if _, _, err := st.SearchVector(ctx, "test", "mm", "", qv[0], "other-space", 5); err == nil {
		t.Fatal("expected search with changed model to be rejected")
	}

	if _, err := st.Migrate(ctx, "test", "mm", []schema.Change{
		{Op: schema.OpSetVectorize, Name: "s", Value: false},
		{Op: schema.OpSetVectorize, Name: "s", Value: true},
	}, other); err != nil {
		t.Fatalf("migrate should re-baseline to the new model: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "mm", []map[string]any{{"s": "more"}}, other); err != nil {
		t.Fatalf("insert after re-baseline: %v", err)
	}
}

func TestChunkedVectorizeBackfill(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "chunky", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	records := make([]map[string]any, 0, 300)
	for i := 0; i < 300; i++ {
		records = append(records, map[string]any{"v": strings.Repeat("a", i+1) + strings.Repeat("b", 300-i)})
	}
	if _, err := st.Insert(ctx, "test", "chunky", records, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := st.Migrate(ctx, "test", "chunky", []schema.Change{
		{Op: schema.OpSetVectorize, Name: "v", Value: true},
	}, testEmbed); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	qv, _ := fakeEmbed(ctx, []string{strings.Repeat("a", 300) + "b"})
	rows, _, err := st.SearchVector(ctx, "test", "chunky", "", qv[0], "fake-space", 1)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(rows) != 1 || rows[0]["v"] != strings.Repeat("a", 300)+"b" {
		t.Fatalf("chunked backfill missed rows: %v", rows)
	}
	if score := rows[0]["_score"].(float64); score < 0.99 {
		t.Fatalf("expected cosine ~1, got %f", score)
	}
}

func TestUnrelatedMigrationPreservesEmbedDim(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "dimkeep", []schema.Field{
		{Name: "s", Type: schema.String, Vectorize: true},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "dimkeep", []map[string]any{{"s": "hello"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	sc, _, err := st.DescribeTable(ctx, "test", "dimkeep")
	if err != nil || sc.EmbedDim != 8 {
		t.Fatalf("expected embed_dim 8 before migrate, got %+v err=%v", sc, err)
	}
	if _, err := st.Migrate(ctx, "test", "dimkeep", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "extra", Type: schema.String}},
	}, testEmbed); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sc, _, err = st.DescribeTable(ctx, "test", "dimkeep")
	if err != nil || sc.EmbedDim != 8 {
		t.Fatalf("unrelated migration must preserve embed_dim, got %+v err=%v", sc, err)
	}
	if _, _, err := st.SearchVector(ctx, "test", "dimkeep", "", []float32{1, 0, 0, 0}, "", 5); err == nil {
		t.Fatal("dim guard must survive unrelated migrations")
	}
}

func TestNoOpVectorizeMigrationSkipsReembed(t *testing.T) {
	st := openStore(t)
	calls := 0
	counting := Embedder{Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		calls++
		return fakeEmbed(ctx, texts)
	}, Identity: "fake-space"}
	if _, err := st.CreateTable(context.Background(), "test", "noop", []schema.Field{
		{Name: "s", Type: schema.String, Vectorize: true},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(context.Background(), "test", "noop", []map[string]any{{"s": "hello world"}}, counting); err != nil {
		t.Fatalf("insert: %v", err)
	}
	before := calls
	if _, err := st.Migrate(context.Background(), "test", "noop", []schema.Change{
		{Op: schema.OpSetVectorize, Name: "s", Value: true},
	}, counting); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if calls != before {
		t.Fatalf("no-op migration re-embedded: %d -> %d", before, calls)
	}
	qv, _ := fakeEmbed(context.Background(), []string{"hello world"})
	rows, _, err := st.SearchVector(context.Background(), "test", "noop", "", qv[0], "fake-space", 1)
	if err != nil || len(rows) != 1 || rows[0]["s"] != "hello world" {
		t.Fatalf("embeddings must survive no-op migration: %v %v", err, rows)
	}
}

func TestMigrateReDerivesEmbedDimAfterDisable(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "redim", []schema.Field{
		{Name: "s", Type: schema.String, Vectorize: true},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "redim", []map[string]any{{"s": "hello"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := st.Migrate(ctx, "test", "redim", []schema.Change{
		{Op: schema.OpSetVectorize, Name: "s", Value: false},
	}, testEmbed); err != nil {
		t.Fatalf("disable: %v", err)
	}
	shortEmbed := func(ctx context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = make([]float32, 4)
			for j := range out[i] {
				out[i][j] = 1
			}
		}
		return out, nil
	}
	shifted := Embedder{Embed: shortEmbed, Identity: "fake-space"}
	if _, err := st.Migrate(ctx, "test", "redim", []schema.Change{
		{Op: schema.OpSetVectorize, Name: "s", Value: true},
	}, shifted); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	sc, _, err := st.DescribeTable(ctx, "test", "redim")
	if err != nil || sc.EmbedDim != 4 {
		t.Fatalf("expected re-derived embed_dim 4, got %+v err=%v", sc, err)
	}
	qv, _ := shortEmbed(ctx, []string{"anything"})
	if _, _, err := st.SearchVector(ctx, "test", "redim", "", qv[0], "fake-space", 5); err != nil {
		t.Fatalf("text-space search after dim change must work: %v", err)
	}
}

func TestBackfillSkipsEmptyStrings(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "empt", []schema.Field{
		{Name: "s", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "empt", []map[string]any{
		{"s": ""}, {"s": "real content"},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := st.Migrate(ctx, "test", "empt", []schema.Change{
		{Op: schema.OpSetVectorize, Name: "s", Value: true},
	}, testEmbed); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT count(*) AS n FROM empt WHERE _embedding IS NULL", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if rows[0]["n"].(int64) != 1 {
		t.Fatalf("empty-string row must stay un-embedded: %v", rows)
	}
}

func TestRequiredFieldAdditionOnPopulatedTableRejected(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "reqadd", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "reqadd", []map[string]any{{"v": "x"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, err := st.Migrate(ctx, "test", "reqadd", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "must", Type: schema.String, Required: true}},
	}, testEmbed)
	if err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected required-field addition on populated table to be rejected, got %v", err)
	}
	if _, err := st.Migrate(ctx, "test", "reqadd", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "opt", Type: schema.String}},
	}, testEmbed); err != nil {
		t.Fatalf("nullable addition must still work: %v", err)
	}
}

func TestRequiredFieldAdditionCarriesNotNull(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "reqempty", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Migrate(ctx, "test", "reqempty", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "must", Type: schema.String, Required: true}},
	}, testEmbed); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rows, _, err := st.Query(ctx, "test", `SELECT "notnull" AS nn FROM pragma_table_info('reqempty') WHERE name = 'must'`, nil)
	if err != nil {
		t.Fatalf("pragma: %v", err)
	}
	if len(rows) != 1 || rows[0]["nn"].(int64) != 1 {
		t.Fatalf("added required field must carry NOT NULL: %v", rows)
	}
}

func TestBackfillRejectsShortProviderResponse(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "shortresp", []schema.Field{
		{Name: "s", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "shortresp", []map[string]any{{"s": "a"}, {"s": "b"}, {"s": "c"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	short := Embedder{Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		return fakeEmbed(ctx, texts[:1])
	}, Identity: "fake-space"}
	_, err := st.Migrate(ctx, "test", "shortresp", []schema.Change{
		{Op: schema.OpSetVectorize, Name: "s", Value: true},
	}, short)
	if err == nil || !strings.Contains(err.Error(), "3 texts") {
		t.Fatalf("expected cardinality error, got %v", err)
	}
}

func TestBackfillRejectsInvalidVectors(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "badvec", []schema.Field{
		{Name: "s", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "badvec", []map[string]any{{"s": "a"}, {"s": "b"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	nan := float32(math.NaN())
	cases := []struct {
		name string
		emb  Embedder
	}{
		{"zero-dim", Embedder{Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
			return make([][]float32, len(texts)), nil
		}, Identity: "fake-space"}},
		{"non-finite", Embedder{Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
			out := make([][]float32, len(texts))
			for i := range out {
				out[i] = []float32{1, nan, 0, 0, 0, 0, 0, 0}
			}
			return out, nil
		}, Identity: "fake-space"}},
	}
	for _, tc := range cases {
		if _, err := st.Migrate(ctx, "test", "badvec", []schema.Change{
			{Op: schema.OpSetVectorize, Name: "s", Value: true},
		}, tc.emb); err == nil {
			t.Fatalf("%s: expected rejection", tc.name)
		}
	}
	sc, _, err := st.DescribeTable(ctx, "test", "badvec")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if sc.Field("s").Vectorize {
		t.Fatalf("rolled-back migration must not persist vectorize: %+v", sc)
	}
}

func TestMigrateRejectsInjectedFieldName(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "victim", []schema.Field{
		{Name: "keep", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "victim", []map[string]any{{"keep": "data"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	injected := `x"; DELETE FROM victim; --`
	_, err := st.Migrate(ctx, "test", "victim", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: injected, Type: schema.String}},
		{Op: schema.OpDropField, Name: injected},
	}, testEmbed)
	if err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected injected field name to be rejected at add time, got %v", err)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT count(*) AS n FROM victim", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if rows[0]["n"].(int64) != 1 {
		t.Fatalf("victim table must be intact: %v", rows)
	}
}

func TestBackfillRequiresEmbedIdentity(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "noid", []schema.Field{
		{Name: "s", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "noid", []map[string]any{{"s": "hello"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	noIdentity := Embedder{Embed: fakeEmbed}
	if _, err := st.Migrate(ctx, "test", "noid", []schema.Change{
		{Op: schema.OpSetVectorize, Name: "s", Value: true},
	}, noIdentity); err == nil {
		t.Fatal("expected identity-less provider to be rejected for backfill")
	}
	sc, _, err := st.DescribeTable(ctx, "test", "noid")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if sc.EmbedSpace != "" {
		t.Fatalf("rolled-back migration must not persist an embed space: %+v", sc)
	}
}

func TestMigrateEnforcesFieldCap(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	fields := make([]schema.Field, MaxFieldsPerTable)
	for i := range fields {
		fields[i] = schema.Field{Name: fmt.Sprintf("f%d", i), Type: schema.String}
	}
	if _, err := st.CreateTable(ctx, "test", "capped", fields); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err := st.Migrate(ctx, "test", "capped", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "one_more", Type: schema.String}},
	}, testEmbed)
	if err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("migration past the field cap must be rejected with ErrInvalid, got %v", err)
	}
}
