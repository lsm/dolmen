package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func mustUpsertByKey(t *testing.T, st *Store, keyFields []string, records []map[string]any) ([]int64, int, int) {
	t.Helper()
	ids, inserted, updated, err := st.UpsertByKey(context.Background(), "test", "notes", keyFields, records, testEmbed)
	if err != nil {
		t.Fatalf("upsert by key: %v", err)
	}
	return ids, inserted, updated
}

func TestUpsertByKeyInsertsThenUpdates(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)

	ids, inserted, updated := mustUpsertByKey(t, st, []string{"title"},
		[]map[string]any{{"title": "alpha", "score": 5}})
	if inserted != 1 || updated != 0 || len(ids) != 1 {
		t.Fatalf("first upsert should insert: ids=%v inserted=%d updated=%d", ids, inserted, updated)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT created_at FROM notes WHERE id = ?", []any{ids[0]}, 0, 0)
	if err != nil {
		t.Fatalf("created_at: %v", err)
	}
	createdAt := rows[0]["created_at"].(string)

	ids2, inserted, updated := mustUpsertByKey(t, st, []string{"title"},
		[]map[string]any{{"title": "alpha", "score": 9, "done": true}})
	if inserted != 0 || updated != 1 || len(ids2) != 1 || ids2[0] != ids[0] {
		t.Fatalf("second upsert should update in place: ids=%v inserted=%d updated=%d", ids2, inserted, updated)
	}

	rows, _, err = st.Query(ctx, "test", "SELECT score, done, created_at, count(*) OVER () AS n FROM notes WHERE title = ?", []any{"alpha"}, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("upsert must not duplicate the row: %v", rows)
	}
	if rows[0]["score"].(int64) != 9 || rows[0]["done"] != true {
		t.Fatalf("update must apply the new values: %v", rows[0])
	}
	if rows[0]["created_at"].(string) != createdAt {
		t.Fatalf("update must keep the original created_at (row updated, not replaced): %v", rows[0])
	}
}

func TestUpsertByKeyPartialUpdateKeepsOtherFields(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)

	mustUpsertByKey(t, st, []string{"title"}, []map[string]any{{"title": "keep", "score": 7, "done": true}})
	mustUpsertByKey(t, st, []string{"title"}, []map[string]any{{"title": "keep"}})

	rows, _, err := st.Query(ctx, "test", "SELECT score, done FROM notes WHERE title = ?", []any{"keep"}, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 || rows[0]["score"].(int64) != 7 || rows[0]["done"] != true {
		t.Fatalf("unspecified fields must keep their values: %v", rows)
	}
}

func TestUpsertByKeyCompositeKey(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)

	key := []string{"title", "score"}
	ids, _, _ := mustUpsertByKey(t, st, key, []map[string]any{{"title": "pair", "score": 1, "done": true}})
	ids2, _, _ := mustUpsertByKey(t, st, key, []map[string]any{{"title": "pair", "score": 1, "done": false}})
	ids3, _, _ := mustUpsertByKey(t, st, key, []map[string]any{{"title": "pair", "score": 2}})
	if ids2[0] != ids[0] {
		t.Fatalf("both key values match, so the same row must update: %v vs %v", ids, ids2)
	}
	if ids3[0] == ids[0] {
		t.Fatalf("a differing key value must insert a new row: %v vs %v", ids, ids3)
	}

	rows, _, err := st.Query(ctx, "test", "SELECT done FROM notes WHERE id = ?", []any{ids[0]}, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 || rows[0]["done"] != false {
		t.Fatalf("the matching key's update must overwrite done: %v", rows)
	}
	rows, _, err = st.Query(ctx, "test", "SELECT count(*) AS n FROM notes WHERE title = ?", []any{"pair"}, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if rows[0]["n"].(int64) != 2 {
		t.Fatalf("expected exactly two rows for the two distinct keys: %v", rows)
	}
}

func TestUpsertByKeyBatchSelfDeduplicates(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)

	ids, inserted, updated := mustUpsertByKey(t, st, []string{"title"}, []map[string]any{
		{"title": "batch", "score": 1},
		{"title": "batch", "score": 2},
	})
	if inserted != 1 || updated != 1 {
		t.Fatalf("a batch with a repeated key should insert then update: inserted=%d updated=%d", inserted, updated)
	}
	if ids[0] != ids[1] {
		t.Fatalf("both records should resolve to the same row: %v", ids)
	}

	rows, _, err := st.Query(ctx, "test", "SELECT score, count(*) OVER () AS n FROM notes WHERE title = ?", []any{"batch"}, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 || rows[0]["score"].(int64) != 2 {
		t.Fatalf("the later record should win, in one row: %v", rows)
	}
}

func TestUpsertByKeyAmbiguousRowsRejected(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)

	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"title": "dup", "score": 1},
		{"title": "dup", "score": 2},
	}, testEmbed); err != nil {
		t.Fatalf("seed duplicate rows: %v", err)
	}
	_, _, _, err := st.UpsertByKey(ctx, "test", "notes", []string{"title"},
		[]map[string]any{{"title": "dup", "score": 3}}, testEmbed)
	if err == nil {
		t.Fatal("a key matching multiple rows is ambiguous and must be rejected")
	}
	if !strings.Contains(err.Error(), "matches multiple") {
		t.Fatalf("error should name the ambiguity, got: %v", err)
	}

	rows, _, err := st.Query(ctx, "test", "SELECT count(*) AS n FROM notes WHERE title = 'dup'", nil, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if rows[0]["n"].(int64) != 2 {
		t.Fatalf("rejected upsert must leave both rows untouched: %v", rows)
	}
}

func TestUpsertByKeyKeyFieldValidation(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)

	rec := []map[string]any{{"title": "x", "score": 1}}
	if _, _, _, err := st.UpsertByKey(ctx, "test", "notes", nil, rec, testEmbed); err == nil {
		t.Fatal("missing key fields must be rejected")
	}
	if _, _, _, err := st.UpsertByKey(ctx, "test", "notes", []string{"nope"}, rec, testEmbed); err == nil {
		t.Fatal("key field outside the schema must be rejected")
	}
	if _, _, _, err := st.UpsertByKey(ctx, "test", "notes", []string{"tags"}, rec, testEmbed); err == nil {
		t.Fatal("json key field must be rejected")
	}
	if _, _, _, err := st.UpsertByKey(ctx, "test", "notes", []string{"emb"}, rec, testEmbed); err == nil {
		t.Fatal("vector key field must be rejected")
	}
	if _, _, _, err := st.UpsertByKey(ctx, "test", "notes", []string{"title", "TITLE"}, rec, testEmbed); err == nil {
		t.Fatal("duplicate key fields (after normalization) must be rejected")
	}
	if _, _, _, err := st.UpsertByKey(ctx, "test", "notes", []string{"title"},
		[]map[string]any{{"score": 1}}, testEmbed); err == nil {
		t.Fatal("record missing the key field must be rejected")
	}
	if _, _, _, err := st.UpsertByKey(ctx, "test", "notes", []string{"title"},
		[]map[string]any{{"title": nil, "score": 1}}, testEmbed); err == nil {
		t.Fatal("null key value must be rejected")
	}
	if _, _, _, err := st.UpsertByKey(ctx, "test", "notes", []string{"title"},
		[]map[string]any{{"title": "x", "bogus": 1}}, testEmbed); err == nil {
		t.Fatal("unknown fields must still be rejected")
	}
	if _, _, _, err := st.UpsertByKey(ctx, "test", "notes", []string{"title"},
		[]map[string]any{{"title": "x", "score": "high"}}, testEmbed); err == nil {
		t.Fatal("type validation must still apply")
	}
}

func TestUpsertByKeyRequiredOnlyOnInsertPath(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	fields := noteFields()
	fields[2].Required = true // score
	if _, err := st.CreateTable(ctx, "test", "req", fields); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, _, _, err := st.UpsertByKey(ctx, "test", "req", []string{"title"},
		[]map[string]any{{"title": "needs-score"}}, testEmbed); err == nil {
		t.Fatal("an unmatched record must satisfy required fields to insert")
	}
	if _, err := st.Insert(ctx, "test", "req", []map[string]any{{"title": "needs-score", "score": 4}}, testEmbed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, _, err := st.UpsertByKey(ctx, "test", "req", []string{"title"},
		[]map[string]any{{"title": "needs-score", "done": true}}, testEmbed); err != nil {
		t.Fatalf("a matched record updates partially and must not re-require fields: %v", err)
	}

	// An explicit null for a required field is invalid input (a 400), not a
	// constraint failure surfacing as a 500.
	_, _, _, err := st.UpsertByKey(ctx, "test", "req", []string{"title"},
		[]map[string]any{{"title": "needs-score", "score": nil}}, testEmbed)
	if err == nil {
		t.Fatal("nulling a required field on the update path must be rejected before SQL")
	}
	if !strings.Contains(err.Error(), "cannot be set to null") {
		t.Fatalf("expected an invalid-request error, got: %v", err)
	}

	rows, _, err := st.Query(ctx, "test", "SELECT score, done FROM req WHERE title = 'needs-score'", nil, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 || rows[0]["score"].(int64) != 4 || rows[0]["done"] != true {
		t.Fatalf("update should apply done and keep score: %v", rows)
	}
}

func TestUpsertByKeyReindexesFulltext(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st) // title and body are fulltext fields

	mustUpsertByKey(t, st, []string{"title"}, []map[string]any{{"title": "fixed", "body": "elephant seal"}})
	hits, _, err := st.SearchFulltext(ctx, "test", "notes", "elephant", 0, 10, false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("inserted row should be findable: %v", hits)
	}

	mustUpsertByKey(t, st, []string{"title"}, []map[string]any{{"title": "fixed", "body": "cheetah"}})
	hits, _, err = st.SearchFulltext(ctx, "test", "notes", "elephant", 0, 10, false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("old text must leave the fulltext index: %v", hits)
	}
	hits, _, err = st.SearchFulltext(ctx, "test", "notes", "cheetah", 0, 10, false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("updated text must be searchable: %v", hits)
	}

	// Nulling an indexed field must drop it from the index (and clear the
	// stale embedding: body is also the vectorized field here).
	mustUpsertByKey(t, st, []string{"title"}, []map[string]any{{"title": "fixed", "body": nil}})
	hits, _, err = st.SearchFulltext(ctx, "test", "notes", "cheetah", 0, 10, false)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("nulled text must leave the fulltext index: %v", hits)
	}
}

func vectorizedKeyTableFields() []schema.Field {
	return []schema.Field{
		{Name: "k", Type: schema.String},
		{Name: "s", Type: schema.Text, Vectorize: true},
		{Name: "n", Type: schema.Number},
	}
}

func TestUpsertByKeyReEmbedsVectorizedField(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "vec", vectorizedKeyTableFields()); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, _, _, err := st.UpsertByKey(ctx, "test", "vec", []string{"k"},
		[]map[string]any{{"k": "a", "s": "apple banana"}}, testEmbed); err != nil {
		t.Fatalf("insert upsert: %v", err)
	}
	rows, _, err := st.Query(ctx, "test", `SELECT "_embedding" AS e FROM vec WHERE k = 'a'`, nil, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	before := rows[0]["e"].(string)
	if before == "" {
		t.Fatal("vectorized upsert must store an embedding")
	}

	if _, _, _, err := st.UpsertByKey(ctx, "test", "vec", []string{"k"},
		[]map[string]any{{"k": "a", "s": "cherry dates"}}, testEmbed); err != nil {
		t.Fatalf("update upsert: %v", err)
	}
	rows, _, err = st.Query(ctx, "test", `SELECT "_embedding" AS e FROM vec WHERE k = 'a'`, nil, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if after := rows[0]["e"].(string); after == "" || after == before {
		t.Fatal("updating a vectorized field must re-embed the new text")
	}

	qvecs, err := fakeEmbed(ctx, []string{"cherry"})
	if err != nil {
		t.Fatalf("embed query: %v", err)
	}
	results, err := st.SearchVector(ctx, "test", "vec", "", qvecs[0], testEmbed.Identity, 0, 10, false, "", nil, nil)
	if err != nil {
		t.Fatalf("vector search: %v", err)
	}
	if len(results.Rows) != 1 || results.Rows[0]["k"] != "a" {
		t.Fatalf("vector search should find the re-embedded row: %v", results.Rows)
	}
}

func TestUpsertByKeyClearsEmbeddingWhenTextEmptied(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "vec", vectorizedKeyTableFields()); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, _, _, err := st.UpsertByKey(ctx, "test", "vec", []string{"k"},
		[]map[string]any{{"k": "a", "s": "hello"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, _, _, err := st.UpsertByKey(ctx, "test", "vec", []string{"k"},
		[]map[string]any{{"k": "a", "s": ""}}, testEmbed); err != nil {
		t.Fatalf("update to empty text: %v", err)
	}
	rows, _, err := st.Query(ctx, "test", `SELECT "_embedding" IS NULL AS emb_null FROM vec WHERE k = 'a'`, nil, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if rows[0]["emb_null"].(int64) != 1 {
		t.Fatal("emptying a vectorized field must clear the stale embedding")
	}
}

func TestUpsertByKeyKeepsEmbeddingWhenFieldAbsent(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "vec", vectorizedKeyTableFields()); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, _, _, err := st.UpsertByKey(ctx, "test", "vec", []string{"k"},
		[]map[string]any{{"k": "a", "s": "hello"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, _, err := st.Query(ctx, "test", `SELECT "_embedding" AS e FROM vec WHERE k = 'a'`, nil, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	before := rows[0]["e"].(string)

	if _, _, _, err := st.UpsertByKey(ctx, "test", "vec", []string{"k"},
		[]map[string]any{{"k": "a", "n": 3}}, testEmbed); err != nil {
		t.Fatalf("update without the vectorized field: %v", err)
	}
	rows, _, err = st.Query(ctx, "test", `SELECT "_embedding" AS e, n FROM vec WHERE k = 'a'`, nil, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if after := rows[0]["e"].(string); after != before {
		t.Fatal("an update that omits the vectorized field must keep the stored embedding")
	}
	if rows[0]["n"].(int64) != 3 {
		t.Fatalf("the supplied field should update: %v", rows[0])
	}
}

func TestUpsertByKeyRejectsProviderChange(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "vec", vectorizedKeyTableFields()); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, _, _, err := st.UpsertByKey(ctx, "test", "vec", []string{"k"},
		[]map[string]any{{"k": "a", "s": "hello"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}
	shifted := Embedder{Embed: fakeEmbed, Identity: "other-space"}
	if _, _, _, err := st.UpsertByKey(ctx, "test", "vec", []string{"k"},
		[]map[string]any{{"k": "a", "s": "world"}}, shifted); err == nil {
		t.Fatal("upsert must reject an embedding-provider change like insert does")
	}
}

func TestUpsertByKeyLegacyKeywordKeyField(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	// A table whose key field is a legacy keyword name ("order") must remain
	// usable as a natural key: the schema lookup verifies the field exists.
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("open namespace: %v", err)
	}
	legacyFields := []schema.Field{{Name: "order", Type: schema.String}, {Name: "qty", Type: schema.Number}}
	if _, err := n.rw.ExecContext(ctx, tableDDL("orders", legacyFields)); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	raw, err := json.Marshal(schema.TableSchema{Namespace: "test", Name: "orders", Version: 1, Fields: legacyFields})
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	if _, err := n.rw.ExecContext(ctx,
		`INSERT INTO _dolmen_tables(name, version, schema_json) VALUES(?,?,?)`,
		"orders", 1, string(raw)); err != nil {
		t.Fatalf("register legacy table: %v", err)
	}

	ids, inserted, updated, err := st.UpsertByKey(ctx, "test", "orders", []string{"order"},
		[]map[string]any{{"order": "first", "qty": 1}}, testEmbed)
	if err != nil {
		t.Fatalf("keyword key field must work: %v", err)
	}
	if inserted != 1 || updated != 0 || len(ids) != 1 {
		t.Fatalf("unexpected first result: ids=%v inserted=%d updated=%d", ids, inserted, updated)
	}
	ids2, inserted, updated, err := st.UpsertByKey(ctx, "test", "orders", []string{"order"},
		[]map[string]any{{"order": "first", "qty": 5}}, testEmbed)
	if err != nil {
		t.Fatalf("keyword key field update: %v", err)
	}
	if inserted != 0 || updated != 1 || ids2[0] != ids[0] {
		t.Fatalf("unexpected second result: ids=%v inserted=%d updated=%d", ids2, inserted, updated)
	}
	rows, _, err := st.Query(ctx, "test", `SELECT qty FROM "orders" WHERE id = ?`, []any{ids[0]}, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if rows[0]["qty"].(int64) != 5 {
		t.Fatalf("update must apply the new value, got %v", rows[0])
	}

	// A key field that does not exist is still rejected by the schema lookup.
	if _, _, _, err := st.UpsertByKey(ctx, "test", "orders", []string{"group"},
		[]map[string]any{{"order": "x"}}, testEmbed); err == nil {
		t.Fatal("expected unknown key field to be rejected")
	}
}
