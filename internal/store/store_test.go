package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func fakeEmbed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, 8)
		for _, r := range []byte(t) {
			v[r%8] += 1
		}
		out[i] = v
	}
	return out, nil
}

var testEmbed = Embedder{Embed: fakeEmbed, Identity: "fake-space"}

func openStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func noteFields() []schema.Field {
	return []schema.Field{
		{Name: "title", Type: schema.String, Fulltext: true},
		{Name: "body", Type: schema.Text, Fulltext: true, Vectorize: true},
		{Name: "score", Type: schema.Number},
		{Name: "done", Type: schema.Boolean},
		{Name: "tags", Type: schema.JSON},
		{Name: "emb", Type: schema.Vector, Dim: 4},
	}
}

func mustCreateNotes(t *testing.T, st *Store) {
	t.Helper()
	if _, err := st.CreateTable(context.Background(), "test", "notes", noteFields()); err != nil {
		t.Fatalf("create table: %v", err)
	}
}

func mustInsertNotes(t *testing.T, st *Store) []int64 {
	t.Helper()
	ids, err := st.Insert(context.Background(), "test", "notes", []map[string]any{
		{"title": "first note", "body": "the dolmen stores stone tables", "score": 5, "done": true, "tags": []any{"a", "b"}, "emb": []any{1.0, 0, 0, 0}},
		{"title": "second note", "body": "agents keep their memory here", "score": 3, "done": false, "emb": []any{0, 1.0, 0, 0}},
		{"title": "third note", "body": "migration and schema evolution", "score": 1, "done": false, "emb": []any{0.9, 0.1, 0, 0}},
	}, testEmbed)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	return ids
}

func TestCreateInsertQuery(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	ids := mustInsertNotes(t, st)
	if len(ids) != 3 {
		t.Fatalf("expected 3 ids, got %d", len(ids))
	}

	rows, err := st.Query(ctx, "test", "SELECT count(*) AS n FROM notes", nil)
	if err != nil {
		t.Fatalf("query count: %v", err)
	}
	if got := rows[0]["n"].(int64); got != 3 {
		t.Fatalf("expected 3 rows, got %d", got)
	}

	rows, err = st.Query(ctx, "test", "SELECT title FROM notes WHERE score > ? ORDER BY score DESC", []any{2})
	if err != nil {
		t.Fatalf("query filter: %v", err)
	}
	if len(rows) != 2 || rows[0]["title"] != "first note" || rows[1]["title"] != "second note" {
		t.Fatalf("unexpected rows: %v", rows)
	}

	if _, err := st.Query(ctx, "test", "DELETE FROM notes", nil); err == nil {
		t.Fatal("expected DELETE via query to be rejected")
	}
	if _, err := st.Query(ctx, "test", "INSERT INTO notes(title) VALUES('x')", nil); err == nil {
		t.Fatal("expected INSERT via query to be rejected")
	}
	if _, err := st.Query(ctx, "test", "SELECT 1; DROP TABLE notes", nil); err == nil {
		t.Fatal("expected multi-statement to be rejected")
	}

	sc, count, err := st.DescribeTable(ctx, "test", "notes")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if sc.Version != 1 || count != 3 || len(sc.Fields) != 6 {
		t.Fatalf("unexpected describe: v%d rows=%d fields=%d", sc.Version, count, len(sc.Fields))
	}
}

func TestInsertValidation(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)

	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"title": "x", "bogus": 1},
	}, testEmbed); err == nil {
		t.Fatal("expected unknown field to be rejected")
	}

	fields := noteFields()
	fields[2].Required = true
	if _, err := st.CreateTable(ctx, "test", "req", fields); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "req", []map[string]any{
		{"title": "x"},
	}, testEmbed); err == nil {
		t.Fatal("expected missing required field to be rejected")
	}

	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"title": "x", "emb": []any{1.0, 0, 0}},
	}, testEmbed); err == nil {
		t.Fatal("expected wrong vector dim to be rejected")
	}

	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"title": "x", "score": "high"},
	}, testEmbed); err == nil {
		t.Fatal("expected wrong scalar type to be rejected")
	}
}

func TestFulltextSearchAndDeleteCascade(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	rows, err := st.SearchFulltext(ctx, "test", "notes", "dolmen", 10)
	if err != nil {
		t.Fatalf("fts: %v", err)
	}
	if len(rows) != 1 || rows[0]["title"] != "first note" {
		t.Fatalf("unexpected fts results: %v", rows)
	}

	deleted, err := st.Delete(ctx, "test", "notes", "done = 1", nil)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 deleted, got %d", deleted)
	}

	rows, err = st.SearchFulltext(ctx, "test", "notes", "dolmen", 10)
	if err != nil {
		t.Fatalf("fts after delete: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected deleted row gone from fts, got %v", rows)
	}
	rows, err = st.SearchFulltext(ctx, "test", "notes", "memory", 10)
	if err != nil {
		t.Fatalf("fts survivor: %v", err)
	}
	if len(rows) != 1 || rows[0]["title"] != "second note" {
		t.Fatalf("unexpected survivor: %v", rows)
	}
}

func TestVectorSearch(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	rows, err := st.SearchVector(ctx, "test", "notes", "emb", []float32{1, 0, 0, 0}, "", 2)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if len(rows) != 2 || rows[0]["title"] != "first note" {
		t.Fatalf("unexpected vector results: %v", rows)
	}
	if score := rows[0]["_score"].(float64); score < 0.99 {
		t.Fatalf("expected cosine ~1, got %f", score)
	}

	if _, err := st.SearchVector(ctx, "test", "notes", "emb", []float32{1, 0}, "", 2); err == nil {
		t.Fatal("expected dim mismatch to be rejected")
	}

	qv, _ := fakeEmbed(ctx, []string{"the dolmen stores stone tables"})
	rows, err = st.SearchVector(ctx, "test", "notes", "", qv[0], "fake-space", 1)
	if err != nil {
		t.Fatalf("vectorize search: %v", err)
	}
	if len(rows) != 1 || rows[0]["title"] != "first note" {
		t.Fatalf("unexpected vectorize results: %v", rows)
	}
}

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

	rows, err := st.SearchFulltext(ctx, "test", "notes", "dolmen", 10)
	if err != nil {
		t.Fatalf("fts after rename: %v", err)
	}
	if len(rows) != 1 || rows[0]["heading"] != "first note" {
		t.Fatalf("fts not rebuilt after rename: %v", rows)
	}

	rows, err = st.Query(ctx, "test", "SELECT priority FROM notes WHERE heading = 'fourth note'", nil)
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
	rows, err := st.SearchVector(ctx, "test", "plain", "", qv[0], "fake-space", 1)
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

func TestTableAndNamespaceValidation(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	if _, err := st.CreateTable(ctx, "test", "BadName", noteFields()); err == nil {
		t.Fatal("expected invalid table name to be rejected")
	}
	if _, err := st.CreateTable(ctx, "../escape", "notes", noteFields()); err == nil {
		t.Fatal("expected invalid namespace to be rejected")
	}
	if _, err := st.CreateTable(ctx, "test", "notes", []schema.Field{
		{Name: "title", Type: "bogus"},
	}); err == nil {
		t.Fatal("expected invalid type to be rejected")
	}
	if _, err := st.CreateTable(ctx, "test", "notes", []schema.Field{
		{Name: "a", Type: schema.Vector},
	}); err == nil {
		t.Fatal("expected vector without dim to be rejected")
	}
	if _, _, err := st.DescribeTable(ctx, "test", "missing"); err == nil {
		t.Fatal("expected missing table to 404")
	}
}

func TestFTSSuffixAndReservedNames(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	if _, err := st.CreateTable(ctx, "test", "notes__fts", noteFields()); err == nil {
		t.Fatal("expected __fts-suffixed table name to be rejected")
	}
	if _, err := st.CreateTable(ctx, "test", "ranked", []schema.Field{
		{Name: "rank", Type: schema.String, Fulltext: true},
	}); err == nil {
		t.Fatal("expected fulltext field named rank to be rejected")
	}
	if _, err := st.CreateTable(ctx, "test", "ranked", []schema.Field{
		{Name: "rank", Type: schema.String},
		{Name: "rowid_like", Type: schema.String},
	}); err != nil {
		t.Fatalf("non-fulltext rank field should be allowed: %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "shadowed", []schema.Field{
		{Name: "rowid", Type: schema.String},
	}); err == nil {
		t.Fatal("expected rowid field name to be rejected")
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
	rows, err := st.SearchVector(ctx, "test", "switch", "", qv[0], "fake-space", 2)
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

func TestLargeDeleteUsesNoInParameterLists(t *testing.T) {
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

	deleted, err := st.Delete(ctx, "test", "big", "1=1", nil)
	if err != nil {
		t.Fatalf("large delete: %v", err)
	}
	if deleted != 1200 {
		t.Fatalf("expected 1200 deleted, got %d", deleted)
	}
	rows, err := st.Query(ctx, "test", "SELECT count(*) AS n FROM big", nil)
	if err != nil {
		t.Fatalf("count after delete: %v", err)
	}
	if got := rows[0]["n"].(int64); got != 0 {
		t.Fatalf("expected 0 rows after delete, got %d", got)
	}
	fts, err := st.SearchFulltext(ctx, "test", "big", "row", 10)
	if err != nil {
		t.Fatalf("fts after delete: %v", err)
	}
	if len(fts) != 0 {
		t.Fatalf("expected fts empty after delete, got %d rows", len(fts))
	}
}

func TestStoragePermissions(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "sec", "t", []schema.Field{
		{Name: "x", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("expected data dir 0700, got %o", info.Mode().Perm())
	}
	dbInfo, err := os.Stat(filepath.Join(dir, "sec.db"))
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}
	if dbInfo.Mode().Perm() != 0o600 {
		t.Fatalf("expected db file 0600, got %o", dbInfo.Mode().Perm())
	}
}

func TestInsertEmptyRecordDefaultValues(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "opts", []schema.Field{
		{Name: "x", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	ids, err := st.Insert(ctx, "test", "opts", []map[string]any{{}}, testEmbed)
	if err != nil {
		t.Fatalf("insert empty record: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("expected 1 id, got %v", ids)
	}
	rows, err := st.Query(ctx, "test", "SELECT count(*) AS n FROM opts", nil)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got := rows[0]["n"].(int64); got != 1 {
		t.Fatalf("expected 1 row, got %d", got)
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
	rows, err := st.SearchVector(ctx, "test", "recyc", "", qv[0], "fake-space", 5)
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
	if _, err := st.SearchVector(ctx, "test", "mm", "", qv[0], "other-space", 5); err == nil {
		t.Fatal("expected search with changed model to be rejected")
	}

	if _, err := st.Migrate(ctx, "test", "mm", []schema.Change{
		{Op: schema.OpSetVectorize, Name: "s", Value: true},
	}, other); err != nil {
		t.Fatalf("migrate should re-baseline to the new model: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "mm", []map[string]any{{"s": "more"}}, other); err != nil {
		t.Fatalf("insert after re-baseline: %v", err)
	}
}

func TestFTSShadowTableNamesRejected(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	for _, name := range []string{"notes__fts", "notes__fts_data", "notes__fts_idx", "notes__fts_content"} {
		if _, err := st.CreateTable(ctx, "test", name, noteFields()); err == nil {
			t.Fatalf("expected shadow-table name %q to be rejected", name)
		}
	}
	for _, name := range []string{"fts_notes", "myfts", "notes_fts"} {
		if _, err := st.CreateTable(ctx, "test", name, noteFields()); err != nil {
			t.Fatalf("expected ordinary name %q to be accepted: %v", name, err)
		}
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
	rows, err := st.SearchVector(ctx, "test", "chunky", "", qv[0], "fake-space", 1)
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

func TestInferCreateInsertRoundTrip(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	samples := []map[string]any{
		{"flag": true, "note": "unknown"},
		{"flag": "maybe", "note": "2"},
	}
	fields := schema.InferFields(samples)
	if _, err := st.CreateTable(ctx, "test", "mixed", fields); err != nil {
		t.Fatalf("create from inferred schema: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "mixed", samples, testEmbed); err != nil {
		t.Fatalf("inserting the same samples must work: %v", err)
	}
	rows, err := st.Query(ctx, "test", "SELECT flag, note FROM mixed ORDER BY id", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 2 || rows[0]["flag"] != "true" || rows[0]["note"] != "unknown" {
		t.Fatalf("unexpected rows: %v", rows)
	}
}

func TestJSONFieldStringScalarsAreValidJSON(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "jf", []schema.Field{
		{Name: "v", Type: schema.JSON},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "jf", []map[string]any{
		{"v": "unknown"},
		{"v": map[string]any{"state": "ok"}},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, err := st.Query(ctx, "test", "SELECT json_extract(v, '$') AS decoded FROM jf ORDER BY id", nil)
	if err != nil {
		t.Fatalf("json_extract over json field: %v", err)
	}
	if rows[0]["decoded"] != "unknown" || rows[1]["decoded"] != `{"state":"ok"}` {
		t.Fatalf("json_extract results wrong: %v", rows)
	}
}

func TestCaseVariantKeyCollisionRejected(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"Title": "Alice", "title": "Bob"},
	}, testEmbed); err == nil {
		t.Fatal("expected case-variant key collision to be rejected")
	}
}
