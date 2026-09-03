package store

import (
	"context"
	"encoding/json"
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
	}, testEmbed, 1)
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

	rows, _, err := st.SearchFulltext(ctx, "test", "notes", "dolmen", 10, false)
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
	}, testEmbed, 0); err != nil {
		t.Fatalf("migrate vectorize: %v", err)
	}

	qv, _ := fakeEmbed(ctx, []string{"hello world"})
	rows, _, err := st.SearchVector(ctx, "test", "plain", "", qv[0], "fake-space", 1, false)
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
	}, testEmbed, 1)
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
	}, testEmbed, 0); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	qv, _ := fakeEmbed(ctx, []string{"delta content here"})
	rows, _, err := st.SearchVector(ctx, "test", "switch", "", qv[0], "fake-space", 2, false)
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
	}, testEmbed, 1); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	if _, err := st.Insert(ctx, "test", "recyc", []map[string]any{
		{"a": "fresh row"},
	}, testEmbed); err != nil {
		t.Fatalf("insert after re-add: %v", err)
	}

	qv, _ := fakeEmbed(ctx, []string{"fresh row"})
	rows, _, err := st.SearchVector(ctx, "test", "recyc", "", qv[0], "fake-space", 5, false)
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
	if _, _, err := st.SearchVector(ctx, "test", "mm", "", qv[0], "other-space", 5, false); err == nil {
		t.Fatal("expected search with changed model to be rejected")
	}

	if _, err := st.Migrate(ctx, "test", "mm", []schema.Change{
		{Op: schema.OpSetVectorize, Name: "s", Value: false},
		{Op: schema.OpSetVectorize, Name: "s", Value: true},
	}, other, 0); err != nil {
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
	}, testEmbed, 0); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	qv, _ := fakeEmbed(ctx, []string{strings.Repeat("a", 300) + "b"})
	rows, _, err := st.SearchVector(ctx, "test", "chunky", "", qv[0], "fake-space", 1, false)
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
	}, testEmbed, 0); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sc, _, err = st.DescribeTable(ctx, "test", "dimkeep")
	if err != nil || sc.EmbedDim != 8 {
		t.Fatalf("unrelated migration must preserve embed_dim, got %+v err=%v", sc, err)
	}
	if _, _, err := st.SearchVector(ctx, "test", "dimkeep", "", []float32{1, 0, 0, 0}, "", 5, false); err == nil {
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
	}, counting, 0); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if calls != before {
		t.Fatalf("no-op migration re-embedded: %d -> %d", before, calls)
	}
	qv, _ := fakeEmbed(context.Background(), []string{"hello world"})
	rows, _, err := st.SearchVector(context.Background(), "test", "noop", "", qv[0], "fake-space", 1, false)
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
	}, testEmbed, 0); err != nil {
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
	}, shifted, 0); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	sc, _, err := st.DescribeTable(ctx, "test", "redim")
	if err != nil || sc.EmbedDim != 4 {
		t.Fatalf("expected re-derived embed_dim 4, got %+v err=%v", sc, err)
	}
	qv, _ := shortEmbed(ctx, []string{"anything"})
	if _, _, err := st.SearchVector(ctx, "test", "redim", "", qv[0], "fake-space", 5, false); err != nil {
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
	}, testEmbed, 0); err != nil {
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
	}, testEmbed, 0)
	if err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected required-field addition on populated table to be rejected, got %v", err)
	}
	if _, err := st.Migrate(ctx, "test", "reqadd", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "opt", Type: schema.String}},
	}, testEmbed, 0); err != nil {
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
	}, testEmbed, 0); err != nil {
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
	}, short, 0)
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
		}, tc.emb, 0); err == nil {
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
	}, testEmbed, 0)
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
	}, noIdentity, 0); err == nil {
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
	}, testEmbed, 0)
	if err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("migration past the field cap must be rejected with ErrInvalid, got %v", err)
	}
}

func TestMigrateAddDropOrderCannotExceedCap(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	fields := make([]schema.Field, MaxFieldsPerTable-1)
	for i := range fields {
		fields[i] = schema.Field{Name: fmt.Sprintf("f%d", i), Type: schema.String}
	}
	if _, err := st.CreateTable(ctx, "test", "almost", fields); err != nil {
		t.Fatalf("create: %v", err)
	}
	changes := []schema.Change{{Op: schema.OpDropField, Name: "f0"}}
	for i := 0; i < 3; i++ {
		changes = append(changes, schema.Change{
			Op:    schema.OpAddField,
			Field: &schema.Field{Name: fmt.Sprintf("extra%d", i), Type: schema.String},
		})
	}
	_, err := st.Migrate(ctx, "test", "almost", changes, testEmbed, 0)
	if err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("adds beyond the cap must be rejected even when a later drop reduces the final count, got %v", err)
	}
}

func TestRequiredFieldAdditionWithDefaultBackfillsEveryType(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "defaults", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "defaults", []map[string]any{
		{"v": "one"}, {"v": "two"},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	sc, err := st.Migrate(ctx, "test", "defaults", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "status", Type: schema.String, Required: true}, Default: "it's 'active'"},
		{Op: schema.OpAddField, Field: &schema.Field{Name: "level", Type: schema.Number, Required: true}, Default: 3.5},
		{Op: schema.OpAddField, Field: &schema.Field{Name: "done", Type: schema.Boolean, Required: true}, Default: true},
		{Op: schema.OpAddField, Field: &schema.Field{Name: "at", Type: schema.Timestamp, Required: true}, Default: "2026-09-01"},
		{Op: schema.OpAddField, Field: &schema.Field{Name: "meta", Type: schema.JSON, Required: true}, Default: map[string]any{"k": []any{1, "x"}}},
		{Op: schema.OpAddField, Field: &schema.Field{Name: "sig", Type: schema.Vector, Dim: 4, Required: true}, Default: []any{0, 0, 1, 0}},
	}, testEmbed, 1)
	if err != nil {
		t.Fatalf("migrate with defaults: %v", err)
	}
	if sc.Version != 2 {
		t.Fatalf("expected version 2, got %d", sc.Version)
	}
	rows, _, err := st.Query(ctx, "test", `SELECT status, level, done, at, meta, sig FROM defaults ORDER BY id`, nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	for i, row := range rows {
		if row["status"] != "it's 'active'" {
			t.Fatalf("row %d: string default must round-trip including quotes: %v", i, row["status"])
		}
		if row["level"].(float64) != 3.5 {
			t.Fatalf("row %d: number default must round-trip: %v", i, row["level"])
		}
		if row["done"] != true {
			t.Fatalf("row %d: boolean default must read true: %v", i, row["done"])
		}
		if row["at"] != "2026-09-01" {
			t.Fatalf("row %d: timestamp default must read canonicalized: %v", i, row["at"])
		}
		meta, ok := row["meta"].(map[string]any)
		if !ok {
			t.Fatalf("row %d: json default must read decoded: %T %v", i, row["meta"], row["meta"])
		}
		if k, ok := meta["k"].([]any); !ok || len(k) != 2 {
			t.Fatalf("row %d: json default payload must round-trip: %v", i, meta)
		} else if kn, ok := k[0].(json.Number); !ok || kn.String() != "1" || k[1] != "x" {
			t.Fatalf("row %d: json default payload must round-trip: %v", i, meta)
		}
		sig, ok := row["sig"].([]float64)
		if !ok || len(sig) != 4 || sig[2] != 1 {
			t.Fatalf("row %d: vector default must read as a number array: %v", i, row["sig"])
		}
	}
	nn, _, err := st.Query(ctx, "test", `SELECT "notnull" AS nn FROM pragma_table_info('defaults') WHERE name IN ('status','level','done','at','meta','sig')`, nil)
	if err != nil {
		t.Fatalf("pragma: %v", err)
	}
	if len(nn) != 6 {
		t.Fatalf("all six defaulted fields must exist: %v", nn)
	}
	for _, r := range nn {
		if r["nn"].(int64) != 1 {
			t.Fatalf("defaulted required fields must carry NOT NULL: %v", r)
		}
	}
	// The insert contract is unchanged: required still means present-in-record.
	if _, err := st.Insert(ctx, "test", "defaults", []map[string]any{{"v": "x"}}, testEmbed); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("insert omitting a required field (with default) must still be rejected, got %v", err)
	}
	if _, err := st.Insert(ctx, "test", "defaults", []map[string]any{{
		"v": "x", "status": "s", "level": 1, "done": false, "at": "2026-09-02", "meta": map[string]any{}, "sig": []any{1, 0, 0, 0},
	}}, testEmbed); err != nil {
		t.Fatalf("insert supplying every required field must pass: %v", err)
	}
}

func TestAddFieldDefaultCoercionFailureLeavesTableUntouched(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "baddef", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "baddef", []map[string]any{{"v": "x"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, err := st.Migrate(ctx, "test", "baddef", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "n", Type: schema.Number, Required: true}, Default: "not a number"},
	}, testEmbed, 1)
	if err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("default that fails type coercion must be rejected, got %v", err)
	}
	sc, _, err := st.DescribeTable(ctx, "test", "baddef")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if sc.Version != 1 || sc.Field("n") != nil {
		t.Fatalf("failed migration must leave the schema untouched: %+v", sc)
	}
	rows, _, err := st.Query(ctx, "test", `SELECT count(*) AS n FROM pragma_table_info('baddef') WHERE name = 'n'`, nil)
	if err != nil {
		t.Fatalf("pragma: %v", err)
	}
	if rows[0]["n"].(int64) != 0 {
		t.Fatal("failed migration must not add the column")
	}
}

func TestAddFieldDefaultRejectedOnNonAddOps(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	_, err := st.Migrate(ctx, "test", "notes", []schema.Change{
		{Op: schema.OpDropField, Name: "tags", Default: "stray"},
	}, testEmbed, 1)
	if err == nil || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "only allowed on add_field") {
		t.Fatalf("default on a non-add_field change must be rejected, got %v", err)
	}
}

func TestMigrateDryRunReportsPlanWithoutSideEffects(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)
	calls := 0
	counting := Embedder{Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		calls++
		return fakeEmbed(ctx, texts)
	}, Identity: "fake-space"}

	plan, err := st.PlanMigration(ctx, "test", "notes", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "prio", Type: schema.Number, Required: true}, Default: 7},
		{Op: schema.OpRenameField, From: "title", To: "heading"},
		{Op: schema.OpDropField, Name: "tags"},
	}, counting, 1)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !plan.DryRun || plan.FromVersion != 1 || plan.ToVersion != 2 {
		t.Fatalf("plan must be marked dry_run with the prospective version: %+v", plan)
	}
	if plan.Table.Version != 2 || plan.Table.Field("heading") == nil || plan.Table.Field("tags") != nil || plan.Table.Field("prio") == nil {
		t.Fatalf("plan must carry the prospective schema: %+v", plan.Table)
	}
	if len(plan.Operations) != 3 || plan.Operations[0] != "add_field prio (number, required, default 7)" {
		t.Fatalf("unexpected operations: %v", plan.Operations)
	}
	if len(plan.Destructive) != 2 {
		t.Fatalf("rename and drop must be flagged destructive: %v", plan.Destructive)
	}
	if plan.BackfillRows != 3 {
		t.Fatalf("backfill_rows must equal the row count, got %d", plan.BackfillRows)
	}
	if !plan.RebuildFulltext || plan.FulltextReindexRows != 3 {
		t.Fatalf("renaming a fulltext field must plan an index rebuild over all rows: %+v", plan)
	}
	if calls != 0 {
		t.Fatalf("dry-run must not call the embedding provider, got %d calls", calls)
	}

	sc, count, err := st.DescribeTable(ctx, "test", "notes")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if sc.Version != 1 || sc.Field("prio") != nil || count != 3 {
		t.Fatalf("dry-run must change nothing: version=%d count=%d", sc.Version, count)
	}
	rows, _, err := st.Query(ctx, "test", `SELECT title FROM notes ORDER BY id LIMIT 1`, nil)
	if err != nil || len(rows) != 1 || rows[0]["title"] != "first note" {
		t.Fatalf("dry-run must leave data intact: %v err=%v", rows, err)
	}
	// The same plan applies cleanly afterwards.
	if _, err := st.Migrate(ctx, "test", "notes", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "prio", Type: schema.Number, Required: true}, Default: 7},
		{Op: schema.OpRenameField, From: "title", To: "heading"},
		{Op: schema.OpDropField, Name: "tags"},
	}, counting, 1); err != nil {
		t.Fatalf("apply after dry-run: %v", err)
	}
}

func TestPlanMigrationEstimatesEmbeddingWorkload(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "estim", []schema.Field{
		{Name: "s", Type: schema.Text},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "estim", []map[string]any{
		{"s": "one"}, {"s": "two"}, {"s": ""}, {"s": nil},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	plan, err := st.PlanMigration(ctx, "test", "estim", []schema.Change{
		{Op: schema.OpSetVectorize, Name: "s", Value: true},
	}, testEmbed, 1)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.EmbedRows != 2 {
		t.Fatalf("embed_rows must count non-empty texts (2 of 4), got %d", plan.EmbedRows)
	}
	if plan.ClearsEmbeddings {
		t.Fatal("first vectorize must not clear embeddings")
	}
	// Enabling on an already-vectorized table re-embeds everything.
	if _, err := st.Migrate(ctx, "test", "estim", []schema.Change{
		{Op: schema.OpSetVectorize, Name: "s", Value: true},
	}, testEmbed, 1); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	plan, err = st.PlanMigration(ctx, "test", "estim", []schema.Change{
		{Op: schema.OpSetVectorize, Name: "s", Value: false},
	}, testEmbed, 2)
	if err != nil {
		t.Fatalf("plan disable: %v", err)
	}
	if !plan.ClearsEmbeddings || plan.EmbedRows != 0 {
		t.Fatalf("disabling vectorize must plan clearing embeddings and no embeds: %+v", plan)
	}
}

func TestPlanMigrationValidatesProviderWithoutCallingIt(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "nprov", []schema.Field{
		{Name: "s", Type: schema.Text},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err := st.PlanMigration(ctx, "test", "nprov", []schema.Change{
		{Op: schema.OpSetVectorize, Name: "s", Value: true},
	}, Embedder{}, 1)
	if err == nil || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "embedding provider") {
		t.Fatalf("dry-run must reject vectorize without a provider, got %v", err)
	}
}

func TestMigrateExpectedVersionConflict(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	for _, fn := range []func() error{
		func() error {
			_, err := st.Migrate(ctx, "test", "notes", []schema.Change{
				{Op: schema.OpAddField, Field: &schema.Field{Name: "prio", Type: schema.Number}},
			}, testEmbed, 5)
			return err
		},
		func() error {
			_, err := st.PlanMigration(ctx, "test", "notes", []schema.Change{
				{Op: schema.OpAddField, Field: &schema.Field{Name: "prio", Type: schema.Number}},
			}, testEmbed, 5)
			return err
		},
	} {
		err := fn()
		var vce *VersionConflictError
		if err == nil || !errors.As(err, &vce) {
			t.Fatalf("stale expected_version must produce a VersionConflictError, got %v", err)
		}
		if vce.CurrentVersion != 1 || vce.ExpectedVersion != 5 {
			t.Fatalf("conflict must carry both versions: %+v", vce)
		}
	}
	if _, err := st.Migrate(ctx, "test", "notes", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "prio", Type: schema.Number}},
	}, testEmbed, 1); err != nil {
		t.Fatalf("matching expected_version must apply: %v", err)
	}
}

func TestMigrateConcurrentVersionCAS(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "race", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	res := make(chan error, 2)
	names := []string{"a", "b"}
	for i, name := range names {
		go func(name string, i int) {
			_, err := st.Migrate(ctx, "test", "race", []schema.Change{
				{Op: schema.OpAddField, Field: &schema.Field{Name: name, Type: schema.String, Required: true}, Default: name},
			}, testEmbed, 1)
			res <- err
		}(name, i)
	}
	errs := []error{<-res, <-res}
	successes, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		default:
			var vce *VersionConflictError
			if !errors.As(err, &vce) {
				t.Fatalf("the loser must fail with a version conflict, got %v", err)
			}
			conflicts++
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("exactly one migrator must commit, got %d successes / %d conflicts", successes, conflicts)
	}
	sc, _, err := st.DescribeTable(ctx, "test", "race")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if sc.Version != 2 {
		t.Fatalf("winner must land version 2, got %d", sc.Version)
	}
	for _, name := range names {
		if (sc.Field(name) != nil) != (successes == 1 && sc.Field(name) != nil) {
			t.Fatalf("unexpected field state: %+v", sc.Fields)
		}
	}
}

func TestListMigrationsNewestFirst(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "hist", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "hist", []map[string]any{{"v": "x"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := st.Migrate(ctx, "test", "hist", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "status", Type: schema.String, Required: true}, Default: "new"},
	}, testEmbed, 1); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if _, err := st.Migrate(ctx, "test", "hist", []schema.Change{
		{Op: schema.OpRenameField, From: "v", To: "value"},
	}, testEmbed, 2); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	ms, err := st.ListMigrations(ctx, "test", "hist")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ms) != 2 {
		t.Fatalf("expected 2 migrations, got %d", len(ms))
	}
	if ms[0].ToVersion != 3 || ms[0].FromVersion != 2 {
		t.Fatalf("newest first: %+v", ms[0])
	}
	if ms[0].Changes[0].Op != schema.OpRenameField || ms[0].Changes[0].From != "v" || ms[0].Changes[0].To != "value" {
		t.Fatalf("rename change must round-trip: %+v", ms[0].Changes[0])
	}
	if ms[1].ToVersion != 2 || ms[1].FromVersion != 1 {
		t.Fatalf("second newest: %+v", ms[1])
	}
	if ms[1].Changes[0].Default != "new" {
		t.Fatalf("default must be recorded in history exactly as applied: %+v", ms[1].Changes[0])
	}
	if ms[0].ID == ms[1].ID || ms[0].At == "" {
		t.Fatalf("entries must carry distinct ids and timestamps: %+v %+v", ms[0], ms[1])
	}
	if _, err := st.ListMigrations(ctx, "test", "nosuch"); err == nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown table must 404, got %v", err)
	}
}

func TestAddFulltextFieldWithDefaultIndexesBackfill(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "ftsdef", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "ftsdef", []map[string]any{
		{"v": "one"}, {"v": "two"},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := st.Migrate(ctx, "test", "ftsdef", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "note", Type: schema.Text, Fulltext: true, Required: true}, Default: "grievance dolmen"},
	}, testEmbed, 1); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rows, _, err := st.SearchFulltext(ctx, "test", "ftsdef", "grievance", 10, false)
	if err != nil {
		t.Fatalf("search over backfilled default: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("both pre-existing rows must be searchable via the backfilled default: %v", rows)
	}
}

func TestPlanEstimatesUsePreMigrationNamesForRenamedVectorField(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "renvec", []schema.Field{
		{Name: "a", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "renvec", []map[string]any{
		{"a": "one"}, {"a": "two"}, {"a": ""},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	changes := []schema.Change{
		{Op: schema.OpRenameField, From: "a", To: "b"},
		{Op: schema.OpSetVectorize, Name: "b", Value: true},
	}
	plan, err := st.PlanMigration(ctx, "test", "renvec", changes, testEmbed, 1)
	if err != nil {
		t.Fatalf("dry-run must resolve the vectorized field back to its pre-migration column: %v", err)
	}
	if plan.EmbedRows != 2 {
		t.Fatalf("embed_rows must count non-empty texts under the old name, got %d", plan.EmbedRows)
	}
	if _, err := st.Migrate(ctx, "test", "renvec", changes, testEmbed, 1); err != nil {
		t.Fatalf("apply after rename must address the old column at plan time: %v", err)
	}
	qv, _ := fakeEmbed(ctx, []string{"two"})
	rows, _, err := st.SearchVector(ctx, "test", "renvec", "", qv[0], "fake-space", 5, false)
	if err != nil || len(rows) < 1 || rows[0]["b"] != "two" {
		t.Fatalf("vectorize after rename must rank the exact text first: %v err=%v", rows, err)
	}
}

func TestListMigrationsPreservesExactNumericDefaults(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "bignum", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "bignum", []map[string]any{{"v": "x"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	big := int64(9007199254740993) // 2^53+1: float64 rounds it down
	if _, err := st.Migrate(ctx, "test", "bignum", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "n", Type: schema.Number, Required: true}, Default: big},
	}, testEmbed, 1); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ms, err := st.ListMigrations(ctx, "test", "bignum")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	num, ok := ms[0].Changes[0].Default.(json.Number)
	if !ok || num.String() != "9007199254740993" {
		t.Fatalf("history must preserve the exact numeric default, got %T %v", ms[0].Changes[0].Default, ms[0].Changes[0].Default)
	}
	rows, _, err := st.Query(ctx, "test", `SELECT n AS n FROM bignum LIMIT 1`, nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if got, ok := rows[0]["n"].(int64); !ok || got != 9007199254740993 {
		t.Fatalf("stored default must be exact, got %T %v", rows[0]["n"], rows[0]["n"])
	}
}

func TestOptionalFieldDefaultDoesNotLeakToFutureInserts(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "optdef", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "optdef", []map[string]any{{"v": "old"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := st.Migrate(ctx, "test", "optdef", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "note", Type: schema.Text, Fulltext: true}, Default: "legacy grievance"},
	}, testEmbed, 1); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Existing rows read the backfill; a fresh insert omitting the field reads NULL.
	rows, _, err := st.Query(ctx, "test", `SELECT note FROM optdef ORDER BY id`, nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if rows[0]["note"] != "legacy grievance" {
		t.Fatalf("existing row must read the backfilled default: %v", rows[0])
	}
	if _, err := st.Insert(ctx, "test", "optdef", []map[string]any{{"v": "new"}}, testEmbed); err != nil {
		t.Fatalf("insert after optional default: %v", err)
	}
	rows, _, err = st.Query(ctx, "test", `SELECT note FROM optdef ORDER BY id`, nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if rows[1]["note"] != nil {
		t.Fatalf("a later insert omitting an optional defaulted field must store NULL, got %v", rows[1]["note"])
	}
	// FTS stays consistent with the base rows: the old row matches the
	// backfilled text, the new row (NULL) does not.
	fts, _, err := st.SearchFulltext(ctx, "test", "optdef", "grievance", 10, false)
	if err != nil {
		t.Fatalf("fts: %v", err)
	}
	if len(fts) != 1 || fts[0]["v"] != "old" {
		t.Fatalf("fulltext must match only the row that actually carries the default text: %v", fts)
	}
}

func TestRenameCycleEstimateTerminates(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "cycle", []schema.Field{
		{Name: "a", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "cycle", []map[string]any{
		{"a": "one"}, {"a": "two"}, {"a": ""},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	changes := []schema.Change{
		{Op: schema.OpRenameField, From: "a", To: "b"},
		{Op: schema.OpRenameField, From: "b", To: "a"},
		{Op: schema.OpSetVectorize, Name: "a", Value: true},
	}
	plan, err := st.PlanMigration(ctx, "test", "cycle", changes, testEmbed, 1)
	if err != nil {
		t.Fatalf("dry-run over a rename cycle must resolve the physical column and return: %v", err)
	}
	if plan.EmbedRows != 2 {
		t.Fatalf("rename cycle must resolve back to the original column, embed_rows got %d", plan.EmbedRows)
	}
	if _, err := st.Migrate(ctx, "test", "cycle", changes, testEmbed, 1); err != nil {
		t.Fatalf("apply over a rename cycle: %v", err)
	}
	qv, _ := fakeEmbed(ctx, []string{"one"})
	rows, _, err := st.SearchVector(ctx, "test", "cycle", "", qv[0], "fake-space", 5, false)
	if err != nil || len(rows) < 1 || rows[0]["a"] != "one" {
		t.Fatalf("vectorize after rename cycle must be searchable: %v err=%v", rows, err)
	}
}

func TestVacatedNameReuseEstimateTracksFieldIdentity(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "vacate", []schema.Field{
		{Name: "a", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "vacate", []map[string]any{
		{"a": "one"}, {"a": "two"}, {"a": ""},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	changes := []schema.Change{
		{Op: schema.OpRenameField, From: "a", To: "b"},
		{Op: schema.OpAddField, Field: &schema.Field{Name: "a", Type: schema.String}},
		{Op: schema.OpSetVectorize, Name: "b", Value: true},
	}
	plan, err := st.PlanMigration(ctx, "test", "vacate", changes, testEmbed, 1)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.EmbedRows != 2 {
		t.Fatalf("vectorizing the renamed field must count its stored texts, not the vacated-name re-add, got %d", plan.EmbedRows)
	}
	if _, err := st.Migrate(ctx, "test", "vacate", changes, testEmbed, 1); err != nil {
		t.Fatalf("apply: %v", err)
	}
	qv, _ := fakeEmbed(ctx, []string{"two"})
	rows, _, err := st.SearchVector(ctx, "test", "vacate", "", qv[0], "fake-space", 5, false)
	if err != nil || len(rows) < 1 || rows[0]["b"] != "two" {
		t.Fatalf("vector search must hit the renamed column's values: %v err=%v", rows, err)
	}
	newA, _, err := st.Query(ctx, "test", `SELECT a FROM vacate WHERE b = 'two'`, nil)
	if err != nil {
		t.Fatalf("query new a: %v", err)
	}
	if newA[0]["a"] != nil {
		t.Fatalf("the re-added field must stay NULL on existing rows: %v", newA[0])
	}
}
