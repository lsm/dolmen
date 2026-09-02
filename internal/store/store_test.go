package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

	rows, _, err := st.Query(ctx, "test", "SELECT count(*) AS n FROM notes", nil)
	if err != nil {
		t.Fatalf("query count: %v", err)
	}
	if got := rows[0]["n"].(int64); got != 3 {
		t.Fatalf("expected 3 rows, got %d", got)
	}

	rows, _, err = st.Query(ctx, "test", "SELECT title FROM notes WHERE score > ? ORDER BY score DESC", []any{2})
	if err != nil {
		t.Fatalf("query filter: %v", err)
	}
	if len(rows) != 2 || rows[0]["title"] != "first note" || rows[1]["title"] != "second note" {
		t.Fatalf("unexpected rows: %v", rows)
	}

	if _, _, err := st.Query(ctx, "test", "DELETE FROM notes", nil); err == nil {
		t.Fatal("expected DELETE via query to be rejected")
	}
	if _, _, err := st.Query(ctx, "test", "INSERT INTO notes(title) VALUES('x')", nil); err == nil {
		t.Fatal("expected INSERT via query to be rejected")
	}
	if _, _, err := st.Query(ctx, "test", "SELECT 1; DROP TABLE notes", nil); err == nil {
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

	rows, _, err := st.SearchFulltext(ctx, "test", "notes", "dolmen", 10)
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

	rows, _, err = st.SearchFulltext(ctx, "test", "notes", "dolmen", 10)
	if err != nil {
		t.Fatalf("fts after delete: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected deleted row gone from fts, got %v", rows)
	}
	rows, _, err = st.SearchFulltext(ctx, "test", "notes", "memory", 10)
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

	rows, _, err := st.SearchVector(ctx, "test", "notes", "emb", []float32{1, 0, 0, 0}, "", 2)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if len(rows) != 2 || rows[0]["title"] != "first note" {
		t.Fatalf("unexpected vector results: %v", rows)
	}
	if score := rows[0]["_score"].(float64); score < 0.99 {
		t.Fatalf("expected cosine ~1, got %f", score)
	}

	if _, _, err := st.SearchVector(ctx, "test", "notes", "emb", []float32{1, 0}, "", 2); err == nil {
		t.Fatal("expected dim mismatch to be rejected")
	}

	qv, _ := fakeEmbed(ctx, []string{"the dolmen stores stone tables"})
	rows, _, err = st.SearchVector(ctx, "test", "notes", "", qv[0], "fake-space", 1)
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
	rows, _, err := st.Query(ctx, "test", "SELECT count(*) AS n FROM big", nil)
	if err != nil {
		t.Fatalf("count after delete: %v", err)
	}
	if got := rows[0]["n"].(int64); got != 0 {
		t.Fatalf("expected 0 rows after delete, got %d", got)
	}
	fts, _, err := st.SearchFulltext(ctx, "test", "big", "row", 10)
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
	rows, _, err := st.Query(ctx, "test", "SELECT count(*) AS n FROM opts", nil)
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
	rows, _, err := st.Query(ctx, "test", "SELECT flag, note FROM mixed ORDER BY id", nil)
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
	rows, _, err := st.Query(ctx, "test", "SELECT json_extract(v, '$') AS decoded FROM jf ORDER BY id", nil)
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

func TestZeroVectorBlobAlwaysBase64InQuery(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"title": "zero", "emb": []any{0, 0, 0, 0}},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT emb FROM notes WHERE title = 'zero'", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	b64, ok := rows[0]["emb"].(string)
	if !ok {
		t.Fatalf("expected string, got %T", rows[0]["emb"])
	}
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(decoded) != 16 {
		t.Fatalf("expected base64 of a 16-byte zero vector, got %q", b64)
	}
	for _, b := range decoded {
		if b != 0 {
			t.Fatalf("expected zero bytes, got %q", b64)
		}
	}
}

func TestRawVectorDimMismatchOnAutoEmbedding(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "auto", []schema.Field{
		{Name: "s", Type: schema.String, Vectorize: true},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "auto", []map[string]any{{"s": "hello"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, _, err := st.SearchVector(ctx, "test", "auto", "", []float32{1, 0, 0, 0}, "", 5); err == nil {
		t.Fatal("expected wrong-length raw vector against auto-embeddings to be rejected")
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

func TestProviderDimChangeRejected(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "dims", []schema.Field{
		{Name: "s", Type: schema.String, Vectorize: true},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "dims", []map[string]any{{"s": "hello"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	shortEmbed := func(ctx context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = make([]float32, 4)
		}
		return out, nil
	}
	shifted := Embedder{Embed: shortEmbed, Identity: "fake-space"}
	if _, err := st.Insert(ctx, "test", "dims", []map[string]any{{"s": "world"}}, shifted); err == nil {
		t.Fatal("expected same-identity dim change to be rejected")
	}
}

func TestVectorFloat32OverflowRejected(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "of", []schema.Field{
		{Name: "v", Type: schema.Vector, Dim: 1},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "of", []map[string]any{{"v": []any{1e300}}}, testEmbed); err == nil {
		t.Fatal("expected out-of-float32-range vector entry to be rejected")
	}
}

func TestQueryResultCap(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "cap", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	for b := 0; b < 2; b++ {
		records := make([]map[string]any, 0, 600)
		for i := 0; i < 600; i++ {
			records = append(records, map[string]any{"v": "x"})
		}
		if _, err := st.Insert(ctx, "test", "cap", records, testEmbed); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	rows, truncated, err := st.Query(ctx, "test", "SELECT * FROM cap", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1000 || !truncated {
		t.Fatalf("expected capped 1000 rows with truncated=true, got %d truncated=%v", len(rows), truncated)
	}
	rows, truncated, err = st.Query(ctx, "test", "SELECT count(*) AS n FROM cap", nil)
	if err != nil || truncated {
		t.Fatalf("small query must not be truncated: %v %v", err, truncated)
	}
	if rows[0]["n"].(int64) != 1200 {
		t.Fatalf("expected 1200 total, got %v", rows[0]["n"])
	}
}

func TestNoOpVectorizeMigrationSkipsReembed(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	calls := 0
	counting := Embedder{Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		calls++
		return fakeEmbed(ctx, texts)
	}, Identity: "fake-space"}
	if _, err := st.CreateTable(ctx, "test", "noop", []schema.Field{
		{Name: "s", Type: schema.String, Vectorize: true},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "noop", []map[string]any{{"s": "hello world"}}, counting); err != nil {
		t.Fatalf("insert: %v", err)
	}
	before := calls
	if _, err := st.Migrate(ctx, "test", "noop", []schema.Change{
		{Op: schema.OpSetVectorize, Name: "s", Value: true},
	}, counting); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if calls != before {
		t.Fatalf("no-op migration re-embedded: %d -> %d", before, calls)
	}
	qv, _ := fakeEmbed(ctx, []string{"hello world"})
	rows, _, err := st.SearchVector(ctx, "test", "noop", "", qv[0], "fake-space", 1)
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

func TestTruncationFlagAccuracy(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "exact", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	records := make([]map[string]any, 0, 1000)
	for i := 0; i < 1000; i++ {
		records = append(records, map[string]any{"v": "x"})
	}
	if _, err := st.Insert(ctx, "test", "exact", records, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, truncated, err := st.Query(ctx, "test", "SELECT * FROM exact", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1000 || truncated {
		t.Fatalf("exactly 1000 rows must not be marked truncated: %d %v", len(rows), truncated)
	}
}

func TestQueryByteBudget(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "bigvals", []schema.Field{
		{Name: "v", Type: schema.Text},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	chunk := strings.Repeat("y", 12<<20)
	for i := 0; i < 4; i++ {
		if _, err := st.Insert(ctx, "test", "bigvals", []map[string]any{{"v": chunk}}, testEmbed); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	rows, truncated, err := st.Query(ctx, "test", "SELECT v FROM bigvals", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 2 || !truncated {
		t.Fatalf("byte budget should cap at 2 of 4 12MiB rows (3rd would exceed 32MiB): %d truncated=%v", len(rows), truncated)
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

func TestSearchByteBudget(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "bigsearch", []schema.Field{
		{Name: "v", Type: schema.Text, Fulltext: true},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	chunk := strings.Repeat("needle ", (12<<20)/7)
	for i := 0; i < 4; i++ {
		if _, err := st.Insert(ctx, "test", "bigsearch", []map[string]any{{"v": chunk}}, testEmbed); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	rows, truncated, err := st.SearchFulltext(ctx, "test", "bigsearch", "needle", 200)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(rows) != 2 || !truncated {
		t.Fatalf("search byte budget should cap at 2 of 4 12MiB rows: %d truncated=%v", len(rows), truncated)
	}
}

func TestValidationRunsBeforeEmbedding(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	calls := 0
	counting := Embedder{Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		calls++
		return fakeEmbed(ctx, texts)
	}, Identity: "fake-space"}
	if _, err := st.CreateTable(ctx, "test", "pree", []schema.Field{
		{Name: "s", Type: schema.String, Vectorize: true},
		{Name: "n", Type: schema.Number},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "pree", []map[string]any{
		{"s": "valid text", "n": "not a number"},
	}, counting); err == nil {
		t.Fatal("expected invalid record to be rejected")
	}
	if calls != 0 {
		t.Fatalf("embedding provider must not be called for invalid records: %d calls", calls)
	}
}

func TestOversizedFirstQueryRowRejected(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "any", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := st.Query(ctx, "test", "SELECT zeroblob(34000000) AS b", nil); err == nil {
		t.Fatal("expected oversized first row to be rejected")
	}
}

func TestNonFiniteQueryValueRejected(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "nf", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := st.Query(ctx, "test", "SELECT 1e999 AS x", nil); err == nil {
		t.Fatal("expected non-finite query value to be rejected")
	}
}

func TestDeleteFilterEvaluatedOnce(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	deleted, err := st.Delete(ctx, "test", "notes", "EXISTS (SELECT 1 FROM notes__fts)", nil)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted != 3 {
		t.Fatalf("filter must be evaluated once: expected 3 deleted, got %d", deleted)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT count(*) AS n FROM notes", nil)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows[0]["n"].(int64) != 0 {
		t.Fatalf("base rows must be deleted alongside the index: %v", rows)
	}
	fts, _, err := st.SearchFulltext(ctx, "test", "notes", "dolmen", 10)
	if err != nil {
		t.Fatalf("search after delete: %v", err)
	}
	if len(fts) != 0 {
		t.Fatalf("expected empty search results, got %v", fts)
	}
}

func TestDuplicateColumnLabelsRejected(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "dup", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := st.Query(ctx, "test", "SELECT 1 AS a, 2 AS a", nil); err == nil {
		t.Fatal("expected duplicate column labels to be rejected")
	}
}

func TestMalformedDeleteFilterIsInvalidRequest(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	_, err := st.Delete(ctx, "test", "notes", "id =", nil)
	if err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected malformed filter to classify as invalid request, got %v", err)
	}
}

func TestOversizedColumnLabelRejected(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "lbl", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	longAlias := strings.Repeat("x", 5000)
	if _, _, err := st.Query(ctx, "test", "SELECT 1 AS \""+longAlias+"\"", nil); err == nil {
		t.Fatal("expected oversized column label to be rejected")
	}
}

func TestMalformedQueryIsInvalidRequest(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "mq", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := st.Query(ctx, "test", "SELECT (", nil); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected malformed SQL to classify as invalid request, got %v", err)
	}
	if _, _, err := st.Query(ctx, "test", "SELECT 1 WHERE 1=?", nil); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected wrong arg count to classify as invalid request, got %v", err)
	}
}

func TestEscapeHeavyStringsBudgeted(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "esc", []schema.Field{
		{Name: "v", Type: schema.Text},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	control := strings.Repeat("\x01", 7<<20)
	if _, err := st.Insert(ctx, "test", "esc", []map[string]any{{"v": control}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, _, err := st.Query(ctx, "test", "SELECT v FROM esc", nil); err == nil {
		t.Fatal("expected escape-heavy row to exceed the response budget")
	}
}

func TestMalformedFTSQueryIsInvalidRequest(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	if _, _, err := st.SearchFulltext(ctx, "test", "notes", "\"unterminated", 10); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected malformed FTS syntax to classify as invalid request, got %v", err)
	}
}

func TestValidateVectorSearchBeforeEmbedding(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if err := st.ValidateVectorSearch(ctx, "test", "missing", "", "fake-space"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not-found for missing table, got %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "vv", []schema.Field{
		{Name: "s", Type: schema.String, Vectorize: true},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "vv", []map[string]any{{"s": "hello"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := st.ValidateVectorSearch(ctx, "test", "vv", "", "other-space"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected identity mismatch to be caught before embedding, got %v", err)
	}
}

func TestIntegerPrecisionPreserved(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "prec", []schema.Field{
		{Name: "n", Type: schema.Number},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "prec", []map[string]any{
		{"n": json.Number("9007199254740993")},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT n FROM prec", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if rows[0]["n"].(int64) != 9007199254740993 {
		t.Fatalf("integer precision lost: %v", rows[0]["n"])
	}
}

func TestEncodedSizeCoversMandatoryEscapes(t *testing.T) {
	if got := encodedSize(" "); got < 6 {
		t.Fatalf("U+2028 should be charged at encoded size, got %d", got)
	}
	if got := encodedSize("\x80"); got < 6 {
		t.Fatalf("invalid UTF-8 should be charged at replacement size, got %d", got)
	}
	if got := encodedSize("plain"); got != 5 {
		t.Fatalf("plain ascii miscounted: %d", got)
	}
}

func TestQueryStepErrorsAreInvalidRequests(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "step", []schema.Field{
		{Name: "v", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := st.Query(ctx, "test", "SELECT json_extract('x', '$') AS v", nil); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected step-time SQL error to classify as invalid request, got %v", err)
	}
}
