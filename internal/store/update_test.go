package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

type countingEmbed struct {
	calls int
	texts []string
}

func (c *countingEmbed) embed(ctx context.Context, texts []string) ([][]float32, error) {
	c.calls++
	c.texts = append(c.texts, texts...)
	return fakeEmbed(ctx, texts)
}

func TestUpdateRewritesRowsAndSearchIndex(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	ids := mustInsertNotes(t, st)

	updated, err := st.Update(ctx, "test", "notes", "done = 0", nil, map[string]any{"title": "renamed"}, testEmbed)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated != 2 {
		t.Fatalf("expected 2 updated, got %d", updated)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT id, title FROM notes ORDER BY id", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for i, want := range []string{"first note", "renamed", "renamed"} {
		if rows[i]["title"] != want {
			t.Fatalf("row %d title = %v, want %q", i, rows[i]["title"], want)
		}
	}
	if rows[0]["id"].(int64) != ids[0] {
		t.Fatalf("update must not renumber ids: %v", rows[0])
	}

	hits, _, err := st.SearchFulltext(ctx, "test", "notes", "renamed", 10, false)
	if err != nil {
		t.Fatalf("fts after update: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 fts hits for the new title, got %v", hits)
	}
	hits, _, err = st.SearchFulltext(ctx, "test", "notes", "second", 10, false)
	if err != nil {
		t.Fatalf("fts old title: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("old title must be gone from the index, got %v", hits)
	}
}

func TestUpdateWithoutIndexChangesLeavesSearchIntact(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	updated, err := st.Update(ctx, "test", "notes", "1=1", nil, map[string]any{"score": 9}, testEmbed)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated != 3 {
		t.Fatalf("expected 3 updated, got %d", updated)
	}
	hits, _, err := st.SearchFulltext(ctx, "test", "notes", "dolmen", 10, false)
	if err != nil {
		t.Fatalf("fts: %v", err)
	}
	if len(hits) != 1 || hits[0]["score"].(int64) != 9 {
		t.Fatalf("non-text update must keep fts rows and expose new values, got %v", hits)
	}
}

func TestUpdateCoercesValues(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	ids := mustInsertNotes(t, st)

	updated, err := st.Update(ctx, "test", "notes", "id = ?", []any{ids[1]}, map[string]any{
		"score": json.Number("2.5"),
		"done":  true,
		"emb":   []any{0.5, 0.5, 0, 0},
		"tags":  []any{"x"},
	}, testEmbed)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated != 1 {
		t.Fatalf("expected 1 updated, got %d", updated)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT score, done, tags FROM notes WHERE id = ?", []any{ids[1]})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if rows[0]["score"].(float64) != 2.5 || rows[0]["done"] != true || !reflect.DeepEqual(rows[0]["tags"], []any{"x"}) {
		t.Fatalf("coerced values wrong: %v", rows[0])
	}

	if _, err := st.Update(ctx, "test", "notes", "1=1", nil, map[string]any{"score": "high"}, testEmbed); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected non-numeric score to be rejected, got %v", err)
	}
	if _, err := st.Update(ctx, "test", "notes", "1=1", nil, map[string]any{"bogus": 1}, testEmbed); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected unknown field to be rejected, got %v", err)
	}
}

func TestUpdateNullClearsOptionalButNotRequired(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	fields := noteFields()
	fields[2].Required = true
	if _, err := st.CreateTable(ctx, "test", "req", fields); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "req", []map[string]any{{"title": "x", "score": 1}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if _, err := st.Update(ctx, "test", "req", "1=1", nil, map[string]any{"score": nil}, testEmbed); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected clearing a required field to be rejected, got %v", err)
	}
	if _, err := st.Update(ctx, "test", "req", "1=1", nil, map[string]any{"tags": nil}, testEmbed); err != nil {
		t.Fatalf("clearing an optional field must work: %v", err)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT tags FROM req", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if _, present := rows[0]["tags"]; !present || rows[0]["tags"] != nil {
		t.Fatalf("expected tags cleared to NULL, got %v", rows[0])
	}
}

func TestUpdateReEmbedsVectorizedField(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	ids := mustInsertNotes(t, st)

	c := &countingEmbed{}
	emb := Embedder{Embed: c.embed, Identity: "fake-space"}

	updated, err := st.Update(ctx, "test", "notes", "title = ?", []any{"first note"},
		map[string]any{"body": "brand new stone text"}, emb)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated != 1 {
		t.Fatalf("expected 1 updated, got %d", updated)
	}
	if c.calls != 1 || len(c.texts) != 1 || c.texts[0] != "brand new stone text" {
		t.Fatalf("expected exactly the new text to be embedded once, got calls=%d texts=%v", c.calls, c.texts)
	}

	qv, err := fakeEmbed(ctx, []string{"brand new stone text"})
	if err != nil {
		t.Fatalf("embed query: %v", err)
	}
	hits, err := st.SearchVector(ctx, "test", "notes", "", qv[0], "fake-space", 10, false, "", nil, nil)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if len(hits.Rows) != 3 || hits.Rows[0]["id"].(int64) != ids[0] {
		t.Fatalf("re-embedded row must be the top hit for its new text, got %v", hits.Rows)
	}
}

func TestUpdateClearsEmbeddingWhenVectorizedFieldCleared(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	updated, err := st.Update(ctx, "test", "notes", "done = 0", nil, map[string]any{"body": nil}, testEmbed)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated != 2 {
		t.Fatalf("expected 2 updated, got %d", updated)
	}
	qv, err := fakeEmbed(ctx, []string{"anything"})
	if err != nil {
		t.Fatalf("embed query: %v", err)
	}
	vres, err := st.SearchVector(ctx, "test", "notes", "", qv[0], "fake-space", 10, false, "", nil, nil)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if len(vres.Rows) != 1 {
		t.Fatalf("cleared rows must lose their embeddings, got %d hits: %v", len(vres.Rows), vres.Rows)
	}

	// body is a fulltext field: cleared text must leave the index, and the
	// untouched row (done = 1) must keep matching its own body text
	hits, _, err := st.SearchFulltext(ctx, "test", "notes", "memory", 10, false)
	if err != nil {
		t.Fatalf("fts cleared: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("cleared body must not match anymore, got %v", hits)
	}
	hits, _, err = st.SearchFulltext(ctx, "test", "notes", "dolmen", 10, false)
	if err != nil {
		t.Fatalf("fts survivor: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("untouched row must stay searchable, got %v", hits)
	}
}

func TestUpdateNoMatchTouchesNothing(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	// a failing provider must not turn a zero-match update into an error:
	// nothing needs embedding, so the provider is never called
	broken := Embedder{Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		return nil, errors.New("provider down")
	}, Identity: "fake-space"}
	updated, err := st.Update(ctx, "test", "notes", "title = ?", []any{"ghost"},
		map[string]any{"body": "ghost text"}, broken)
	if err != nil {
		t.Fatalf("zero-match update must not call the embedding provider: %v", err)
	}
	if updated != 0 {
		t.Fatalf("expected 0 updated, got %d", updated)
	}
	sc, _, err := st.DescribeTable(ctx, "test", "notes")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if sc.EmbedSpace != "fake-space" || sc.EmbedDim != 8 {
		t.Fatalf("no-match update must not rewrite embedding metadata, got space=%q dim=%d", sc.EmbedSpace, sc.EmbedDim)
	}
	hits, _, err := st.SearchFulltext(ctx, "test", "notes", "ghost", 10, false)
	if err != nil {
		t.Fatalf("fts: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("no-match update must not leak into the index, got %v", hits)
	}
}

func TestUpdateEmbeddingGuardRails(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	if _, err := st.Update(ctx, "test", "notes", "1=1", nil, map[string]any{"body": "x"},
		Embedder{Embed: fakeEmbed, Identity: "other-space"}); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected provider-change rejection, got %v", err)
	}
	if _, err := st.Update(ctx, "test", "notes", "1=1", nil, map[string]any{"body": "x"},
		Embedder{}); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected missing-provider rejection, got %v", err)
	}
}

func TestUpdateValidation(t *testing.T) {
	st := openStore(t)
	mustCreateNotes(t, st)

	if _, err := st.Update(context.Background(), "test", "notes", "", nil, map[string]any{"title": "x"}, testEmbed); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected empty filter to be rejected, got %v", err)
	}
	if _, err := st.Update(context.Background(), "test", "notes", "1=1; DROP TABLE notes", nil, map[string]any{"title": "x"}, testEmbed); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected semicolon in filter to be rejected, got %v", err)
	}
	if _, err := st.Update(context.Background(), "test", "notes", "1=1", nil, map[string]any{}, testEmbed); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected empty set to be rejected, got %v", err)
	}
	if _, err := st.Update(context.Background(), "test", "notes", "id =", nil, map[string]any{"title": "x"}, testEmbed); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected malformed filter to classify as invalid request, got %v", err)
	}
}

func TestUpsertUpdatesWhenMatched(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	res, err := st.Upsert(ctx, "test", "notes", "done = 1", nil, map[string]any{"score": 42}, testEmbed)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if res.Inserted || res.ID != 0 || res.Updated != 1 {
		t.Fatalf("expected matched upsert to update exactly one row, got %+v", res)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT count(*) AS n FROM notes WHERE score = 42", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if rows[0]["n"].(int64) != 1 {
		t.Fatalf("expected the matched row updated, got %v", rows)
	}
}

func TestUpsertInsertsWhenNoMatch(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	mustInsertNotes(t, st)

	res, err := st.Upsert(ctx, "test", "notes", "title = ?", []any{"ghost"},
		map[string]any{"title": "ghost note", "body": "haunting new text", "score": 7, "done": true}, testEmbed)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if !res.Inserted || res.ID == 0 || res.Updated != 0 {
		t.Fatalf("expected unmatched upsert to insert, got %+v", res)
	}

	rows, _, err := st.Query(ctx, "test", "SELECT title, score FROM notes WHERE id = ?", []any{res.ID})
	if err != nil || len(rows) != 1 {
		t.Fatalf("query inserted row: %v %v", rows, err)
	}
	if rows[0]["title"] != "ghost note" || rows[0]["score"].(int64) != 7 {
		t.Fatalf("inserted row wrong: %v", rows[0])
	}
	hits, _, err := st.SearchFulltext(ctx, "test", "notes", "haunting", 10, false)
	if err != nil {
		t.Fatalf("fts: %v", err)
	}
	if len(hits) != 1 || hits[0]["id"].(int64) != res.ID {
		t.Fatalf("inserted row must be searchable, got %v", hits)
	}
	qv, err := fakeEmbed(ctx, []string{"haunting new text"})
	if err != nil {
		t.Fatalf("embed query: %v", err)
	}
	vhits, err := st.SearchVector(ctx, "test", "notes", "", qv[0], "fake-space", 10, false, "", nil, nil)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if len(vhits.Rows) != 4 || vhits.Rows[0]["id"].(int64) != res.ID {
		t.Fatalf("inserted row must be embedded and rank first for its text, got %v", vhits.Rows)
	}
}

func TestUpsertInsertEnforcesRequiredFields(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	fields := noteFields()
	fields[2].Required = true
	if _, err := st.CreateTable(ctx, "test", "req", fields); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := st.Upsert(ctx, "test", "req", "1=0", nil, map[string]any{"title": "no score"}, testEmbed); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected insert path to enforce required fields, got %v", err)
	}
	// the candidate is validated before the embedding provider is called:
	// even a failing provider must surface the required-field error, not an
	// embedding failure
	broken := Embedder{Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		return nil, errors.New("provider down")
	}, Identity: "fake-space"}
	_, err := st.Upsert(ctx, "test", "req", "1=0", nil, map[string]any{"body": "text but no score"}, broken)
	if err == nil || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), `"score" is required`) {
		t.Fatalf("expected the required-field error before any embedding, got %v", err)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT count(*) AS n FROM req", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if rows[0]["n"].(int64) != 0 {
		t.Fatalf("failed upsert must not leave a row behind, got %v", rows)
	}

	res, err := st.Upsert(ctx, "test", "req", "1=0", nil, map[string]any{"title": "scored", "score": 1}, testEmbed)
	if err != nil {
		t.Fatalf("upsert with required field satisfied: %v", err)
	}
	if !res.Inserted {
		t.Fatalf("expected insert, got %+v", res)
	}
}

func TestUpdatePlainTableWithoutIndexes(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "plain", []schema.Field{
		{Name: "a", Type: schema.String},
		{Name: "n", Type: schema.Number},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "plain", []map[string]any{{"a": "x", "n": 1}, {"a": "y", "n": 2}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	updated, err := st.Update(ctx, "test", "plain", "a = 'x'", nil, map[string]any{"a": "z", "n": 9}, testEmbed)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated != 1 {
		t.Fatalf("expected 1 updated, got %d", updated)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT a, n FROM plain ORDER BY id", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if rows[0]["a"] != "z" || rows[0]["n"].(int64) != 9 || rows[1]["a"] != "y" {
		t.Fatalf("plain update wrong: %v", rows)
	}

	// a table with no vectorize field must not gain embedding metadata on update
	sc, _, err := st.DescribeTable(ctx, "test", "plain")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if sc.EmbedSpace != "" || sc.EmbedDim != 0 {
		t.Fatalf("plain table must not gain embedding metadata, got space=%q dim=%d", sc.EmbedSpace, sc.EmbedDim)
	}
}
