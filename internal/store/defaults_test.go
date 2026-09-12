package store

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/schema"
)

// jsonNum builds the json.Number the API decoders produce, so tests exercise
// defaults exactly as they arrive over the wire.
func jsonNum(s string) json.Number {
	return json.Number(s)
}

func defaultFields() []schema.Field {
	return []schema.Field{
		{Name: "title", Type: schema.String, Fulltext: true, Default: "untitled"},
		{Name: "score", Type: schema.Number, Default: jsonNum("10")},
		{Name: "done", Type: schema.Boolean, Default: true},
		{Name: "status", Type: schema.String},
		{Name: "seen_at", Type: schema.Timestamp, Default: "2026-01-02T03:04:05Z"},
		{Name: "meta", Type: schema.JSON, Default: map[string]any{"k": "v"}},
		{Name: "emb", Type: schema.Vector, Dim: 2, Default: []any{jsonNum("1"), jsonNum("0")}},
	}
}

func TestCreateTableDefaultValidation(t *testing.T) {
	st := openStore(t)
	mustNS(t, st, "test")
	ctx := context.Background()

	cases := []struct {
		name   string
		fields []schema.Field
		want   string
	}{
		{
			name:   "required_and_default_exclusive",
			fields: []schema.Field{{Name: "a", Type: schema.String, Required: true, Default: "x"}},
			want:   "not allowed on required",
		},
		{
			name:   "vectorize_rejects_default",
			fields: []schema.Field{{Name: "a", Type: schema.Text, Vectorize: true, Default: "x"}},
			want:   "not allowed on vectorize",
		},
		{
			// Numbers coerce to strings exactly like insert values do, so the
			// mismatch case needs a non-scalar default.
			name:   "default_must_match_type",
			fields: []schema.Field{{Name: "a", Type: schema.String, Default: true}},
			want:   "expected a string",
		},
		{
			name:   "timestamp_default_must_parse",
			fields: []schema.Field{{Name: "a", Type: schema.Timestamp, Default: "yesterday"}},
			want:   "expected an ISO/RFC3339 timestamp",
		},
		{
			name:   "vector_default_must_match_dim",
			fields: []schema.Field{{Name: "a", Type: schema.Vector, Dim: 2, Default: []any{jsonNum("1")}}},
			want:   "expected dim 2",
		},
		{
			name:   "default_must_not_contain_nul",
			fields: []schema.Field{{Name: "a", Type: schema.String, Default: "a\x00b"}},
			want:   "NUL",
		},
		{
			name:   "now_default_rejected_on_string_field",
			fields: []schema.Field{{Name: "a", Type: schema.String, Default: schema.NowDefault}},
			want:   `"now()" is only allowed on timestamp fields`,
		},
		{
			name:   "now_default_rejected_on_number_field",
			fields: []schema.Field{{Name: "a", Type: schema.Number, Default: schema.NowDefault}},
			want:   `"now()" is only allowed on timestamp fields`,
		},
	}
	for _, tc := range cases {
		_, err := st.CreateTable(ctx, "test", tc.name, tc.fields)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: expected error containing %q, got %v", tc.name, tc.want, err)
		}
	}
}

func TestInsertAppliesDeclaredDefaults(t *testing.T) {
	st := openStore(t)
	mustNS(t, st, "test")
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "things", defaultFields()); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Describe reflects the declared defaults.
	sc, _, err := st.DescribeTable(ctx, "test", "things")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if got := sc.Field("title").Default; got != "untitled" {
		t.Fatalf(`describe: title default = %v, want "untitled"`, got)
	}
	if got := sc.Field("score").Default; got != jsonNum("10") {
		t.Fatalf("describe: score default = %T(%v), want json.Number 10", got, got)
	}

	// Omitted fields store their defaults; provided fields win; explicit null
	// stays null.
	ids, err := st.Insert(ctx, "test", "things", []map[string]any{
		{"title": "named", "status": "new"},
		{"score": jsonNum("42")},
		{"title": "cleared", "meta": nil},
	}, Embedder{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	rows, _, err := st.Query(ctx, "test",
		"SELECT title, score, done, status, seen_at, meta, emb FROM things WHERE id = ?", []any{ids[0]}, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("query: %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row["title"] != "named" || row["score"] != int64(10) || row["status"] != "new" {
		t.Fatalf("provided values must win and omitted ones default, got %v", row)
	}
	if row["done"] != true {
		t.Fatalf("omitted boolean must store its default, got %v", row["done"])
	}
	if row["seen_at"] != "2026-01-02T03:04:05Z" {
		t.Fatalf("omitted timestamp must store its canonical default, got %v", row["seen_at"])
	}
	if meta, ok := row["meta"].(map[string]any); !ok || meta["k"] != "v" {
		t.Fatalf("omitted json must store its default, got %v", row["meta"])
	}
	if vec, ok := row["emb"].([]float64); !ok || len(vec) != 2 || vec[0] != 1 {
		t.Fatalf("omitted vector must store its default, got %v", row["emb"])
	}

	rows, _, err = st.Query(ctx, "test",
		"SELECT title, score, done FROM things WHERE id = ?", []any{ids[1]}, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if rows[0]["title"] != "untitled" || rows[0]["score"] != int64(42) || rows[0]["done"] != true {
		t.Fatalf("second row must mix defaults with provided values, got %v", rows[0])
	}

	rows, _, err = st.Query(ctx, "test",
		"SELECT meta FROM things WHERE id = ?", []any{ids[2]}, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if rows[0]["meta"] != nil {
		t.Fatalf("explicit null must stay null, not default, got %v", rows[0]["meta"])
	}
}

func TestInsertDefaultsFeedFulltextIndex(t *testing.T) {
	st := openStore(t)
	mustNS(t, st, "test")
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "notes", []schema.Field{
		{Name: "body", Type: schema.Text, Fulltext: true, Default: "standard operating procedure"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{{}}, Embedder{}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	res, _, err := st.SearchFulltext(ctx, "test", "notes", "operating", 0, 10, false, "", nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("defaulted fulltext value must be indexed and searchable, got %d results", len(res))
	}
}

func TestNumericDefaultExactAcrossSchemaReload(t *testing.T) {
	st := openStore(t)
	mustNS(t, st, "test")
	ctx := context.Background()
	big := "9007199254740993" // 2^53+1: inexact through float64
	if _, err := st.CreateTable(ctx, "test", "exact", []schema.Field{
		{Name: "n", Type: schema.Number, Default: jsonNum(big)},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A fresh Store forces the schema_json round-trip; describe must report
	// the default exactly as declared, not the nearest float64.
	raw2, err := Open(st.dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	st2 := legacy(raw2)
	defer st2.Close()
	sc, _, err := st2.DescribeTable(ctx, "test", "exact")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if got := sc.Field("n").Default; got != jsonNum(big) {
		t.Fatalf("default must round-trip exactly, got %T(%v), want json.Number(%s)", got, got, big)
	}
	ids, err := st2.Insert(ctx, "test", "exact", []map[string]any{{}}, Embedder{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, _, err := st2.Query(ctx, "test", "SELECT n FROM exact WHERE id = ?", []any{ids[0]}, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if got, ok := rows[0]["n"].(int64); !ok || got != 9007199254740993 {
		t.Fatalf("stored default must be exact, got %T(%v)", rows[0]["n"], rows[0]["n"])
	}
}

func TestUpsertPathsApplyDefaults(t *testing.T) {
	st := openStore(t)
	mustNS(t, st, "test")
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "items", []schema.Field{
		{Name: "sku", Type: schema.String},
		{Name: "qty", Type: schema.Number, Default: jsonNum("1")},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	// upsert_by_key insert branch stores the default, and its update branch
	// keeps the stored value instead of re-defaulting.
	for i := 0; i < 2; i++ {
		if _, _, _, err := st.UpsertByKey(ctx, "test", "items", []string{"sku"},
			[]map[string]any{{"sku": "a"}}, Embedder{}); err != nil {
			t.Fatalf("upsert_by_key %d: %v", i, err)
		}
	}
	if _, _, _, err := st.UpsertByKey(ctx, "test", "items", []string{"sku"},
		[]map[string]any{{"sku": "a", "qty": jsonNum("5")}}, Embedder{}); err != nil {
		t.Fatalf("upsert_by_key update: %v", err)
	}
	if _, _, _, err := st.UpsertByKey(ctx, "test", "items", []string{"sku"},
		[]map[string]any{{"sku": "a"}}, Embedder{}); err != nil {
		t.Fatalf("upsert_by_key re-run: %v", err)
	}

	// Filter upsert: the insert branch stores the default; matched updates
	// never re-default. An explicit null clears, and a later insert still
	// defaults.
	if _, err := st.Upsert(ctx, "test", "items", "sku = 'b'",
		nil, map[string]any{"sku": "b"}, Embedder{}); err != nil {
		t.Fatalf("upsert insert: %v", err)
	}
	if _, err := st.Upsert(ctx, "test", "items", "sku = 'b'",
		nil, map[string]any{"qty": jsonNum("9")}, Embedder{}); err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	if _, err := st.Update(ctx, "test", "items", "sku = 'a'",
		nil, map[string]any{"qty": nil}, Embedder{}); err != nil {
		t.Fatalf("update nulls qty: %v", err)
	}
	if _, err := st.Upsert(ctx, "test", "items", "sku = 'c'",
		nil, map[string]any{"sku": "c"}, Embedder{}); err != nil {
		t.Fatalf("upsert insert after null: %v", err)
	}

	rows, _, err := st.Query(ctx, "test",
		"SELECT sku, qty FROM items ORDER BY id", nil, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	want := []struct {
		sku string
		qty any
	}{
		{"a", nil}, // update set null; later matched upserts keep it
		{"b", int64(9)},
		{"c", int64(1)}, // insert after the null still defaults
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d: %v", len(rows), len(want), rows)
	}
	for i, w := range want {
		if rows[i]["sku"] != w.sku || rows[i]["qty"] != w.qty {
			t.Fatalf("row %d = %v, want %v", i, rows[i], w)
		}
	}
}

func TestMigrateRejectsFieldLevelDefault(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	_, err := st.Migrate(ctx, "test", "notes", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "extra", Type: schema.String, Default: "x"}},
	}, Embedder{}, 0)
	if err == nil || !strings.Contains(err.Error(), "not inside field") {
		t.Fatalf("expected add_field field-level default to be rejected, got %v", err)
	}
}

func TestMigrateGrandfathersLegacyNowDefault(t *testing.T) {
	st := openStore(t)
	mustNS(t, st, "test")
	ctx := context.Background()
	sc, err := st.CreateTable(ctx, "test", "legacy", []schema.Field{
		{Name: "title", Type: schema.String},
		{Name: "tag", Type: schema.String, Default: "draft"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	sc.Field("tag").Default = schema.NowDefault
	raw, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	if _, err := n.rw.ExecContext(ctx,
		`UPDATE _dolmen_tables SET schema_json = ? WHERE name = ?`, string(raw), "legacy"); err != nil {
		t.Fatalf("inject pre-release schema: %v", err)
	}

	if _, err := st.Migrate(ctx, "test", "legacy", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "extra", Type: schema.String}},
	}, Embedder{}, 1); err != nil {
		t.Fatalf("migrate on a legacy now()-defaulted table must succeed: %v", err)
	}

	ids, err := st.Insert(ctx, "test", "legacy", []map[string]any{{"title": "x"}}, Embedder{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT tag FROM legacy WHERE id = ?", []any{ids[0]}, 0, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("query: %v rows=%d", err, len(rows))
	}
	if rows[0]["tag"] != schema.NowDefault {
		t.Fatalf("the legacy static default must keep storing the literal, got %v", rows[0]["tag"])
	}
}

func TestInsertRetryAfterDefaultedFieldDropped(t *testing.T) {
	st := openStore(t)
	mustNS(t, st, "test")
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "docs", []schema.Field{
		{Name: "body", Type: schema.Text, Vectorize: true},
		{Name: "tag", Type: schema.String, Default: "draft"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	calls := 0
	emb := Embedder{Identity: "fake-space", Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		calls++
		if calls == 1 {
			// Land a schema change inside attempt 1's embedding pause: the
			// in-transaction version check then forces a retry, which must see
			// the caller's records exactly as sent — not fields defaulted by
			// the stale attempt (the dropped field would fail as unknown).
			if _, err := st.Migrate(ctx, "test", "docs", []schema.Change{
				{Op: schema.OpDropField, Name: "tag"},
			}, Embedder{}, 1); err != nil {
				return nil, err
			}
		}
		return fakeEmbed(ctx, texts)
	}}

	ids, err := st.Insert(ctx, "test", "docs", []map[string]any{{"body": "hello world"}}, emb)
	if err != nil {
		t.Fatalf("insert after concurrent drop of a defaulted field: %v", err)
	}
	if calls < 2 {
		t.Fatalf("expected the schema change to force a retry, got %d embed calls", calls)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT body FROM docs WHERE id = ?", []any{ids[0]}, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 || rows[0]["body"] != "hello world" {
		t.Fatalf("retried insert must land its record, got %v", rows)
	}
}

var nowStampRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)

func mustParseNowStamp(t *testing.T, v any) time.Time {
	t.Helper()
	s, ok := v.(string)
	if !ok || !nowStampRe.MatchString(s) {
		t.Fatalf("server now-stamp must be a millisecond-precision UTC timestamp, got %#v", v)
	}
	ts, err := time.Parse(nowStampLayout, s)
	if err != nil {
		t.Fatalf("parse now-stamp %q: %v", s, err)
	}
	return ts
}

func TestCreateTableNowDefault(t *testing.T) {
	st := openStore(t)
	mustNS(t, st, "test")
	ctx := context.Background()
	sc, err := st.CreateTable(ctx, "test", "stamped", []schema.Field{
		{Name: "title", Type: schema.String},
		{Name: "updated_at", Type: schema.Timestamp, Default: schema.NowDefault},
	})
	if err != nil {
		t.Fatalf("create with a now() default on a timestamp field: %v", err)
	}
	if got := sc.Field("updated_at").Default; got != schema.NowDefault {
		t.Fatalf("describe must report the declared default verbatim, got %v", got)
	}
	raw2, err := Open(st.dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	st2 := legacy(raw2)
	defer st2.Close()
	sc2, _, err := st2.DescribeTable(ctx, "test", "stamped")
	if err != nil {
		t.Fatalf("describe after reopen: %v", err)
	}
	if got := sc2.Field("updated_at").Default; got != schema.NowDefault {
		t.Fatalf("now() default must survive the schema_json round-trip, got %v", got)
	}
}

func TestInsertNowDefaultStampsServerTime(t *testing.T) {
	st := openStore(t)
	mustNS(t, st, "test")
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "events", []schema.Field{
		{Name: "name", Type: schema.String},
		{Name: "updated_at", Type: schema.Timestamp, Default: schema.NowDefault},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	before := time.Now()
	ids, err := st.Insert(ctx, "test", "events", []map[string]any{
		{"name": "omitted"},
		{"name": "supplied", "updated_at": "2026-01-02T03:04:05Z"},
		{"name": "nulled", "updated_at": nil},
	}, Embedder{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	after := time.Now()
	if len(ids) != 3 {
		t.Fatalf("insert returned %d ids, want 3", len(ids))
	}

	rows, _, err := st.Query(ctx, "test", "SELECT name, updated_at FROM events ORDER BY id", nil, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3: %v", len(rows), rows)
	}
	stamp := mustParseNowStamp(t, rows[0]["updated_at"])
	if stamp.Before(before.Add(-2*time.Second)) || stamp.After(after.Add(2*time.Second)) {
		t.Fatalf("omitted field must store the server's current time (between %v and %v), got %v", before, after, stamp)
	}
	if rows[1]["updated_at"] != "2026-01-02T03:04:05Z" {
		t.Fatalf("supplied timestamp must win over the now() default, got %v", rows[1]["updated_at"])
	}
	if rows[2]["updated_at"] != nil {
		t.Fatalf("explicit null must stay null, not stamp now, got %v", rows[2]["updated_at"])
	}
}

func TestInsertNowDefaultStampsAfterEmbedding(t *testing.T) {
	st := openStore(t)
	mustNS(t, st, "test")
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "vecevents", []schema.Field{
		{Name: "body", Type: schema.Text, Vectorize: true},
		{Name: "updated_at", Type: schema.Timestamp, Default: schema.NowDefault},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	var embDone time.Time
	emb := Embedder{Identity: "fake-space", Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		time.Sleep(50 * time.Millisecond)
		embDone = time.Now()
		return fakeEmbed(ctx, texts)
	}}
	before := time.Now()
	ids, err := st.Insert(ctx, "test", "vecevents", []map[string]any{{"body": "hello"}}, emb)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if embDone.Before(before.Add(40 * time.Millisecond)) {
		t.Fatalf("embedding must have paused the insert, started %v done %v", before, embDone)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT updated_at FROM vecevents WHERE id = ?", []any{ids[0]}, 0, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("query: %v rows=%d", err, len(rows))
	}
	stamp := mustParseNowStamp(t, rows[0]["updated_at"])
	if stamp.Before(embDone.Add(-2 * time.Millisecond)) {
		t.Fatalf("now() must resolve at write time, after the embedding pause: embedder done %v, stamp %v", embDone, stamp)
	}
}

func TestInsertNowDefaultIdempotentReplay(t *testing.T) {
	st := openStore(t)
	mustNS(t, st, "test")
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "tasks", []schema.Field{
		{Name: "title", Type: schema.String},
		{Name: "updated_at", Type: schema.Timestamp, Default: schema.NowDefault},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	rec := []map[string]any{{"title": "retry me"}}
	ids1, replayed, err := st.InsertIdempotent(ctx, "test", "tasks", rec, Embedder{}, "now-key")
	if err != nil || replayed {
		t.Fatalf("first insert: %v replayed=%v", err, replayed)
	}
	readStamp := func() string {
		rows, _, err := st.Query(ctx, "test", "SELECT updated_at FROM tasks WHERE id = ?", []any{ids1[0]}, 0, 0)
		if err != nil || len(rows) != 1 {
			t.Fatalf("read stamp: %v rows=%d", err, len(rows))
		}
		return rows[0]["updated_at"].(string)
	}
	stamp1 := readStamp()
	mustParseNowStamp(t, stamp1)

	ids2, replayed, err := st.InsertIdempotent(ctx, "test", "tasks", rec, Embedder{}, "now-key")
	if err != nil {
		t.Fatalf("retry with the identical body must replay, not error: %v", err)
	}
	if !replayed {
		t.Fatal("retry with a recorded key must report replayed")
	}
	if len(ids2) != 1 || ids2[0] != ids1[0] {
		t.Fatalf("retry must return the original ids, got %v want %v", ids2, ids1)
	}
	if stamp2 := readStamp(); stamp2 != stamp1 {
		t.Fatalf("replay must keep the original stamp (no re-stamp), got %q want %q", stamp2, stamp1)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT count(*) AS n FROM tasks", nil, 0, 0)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows[0]["n"].(int64) != 1 {
		t.Fatalf("retry must not insert another row: %v", rows)
	}
}

func TestUpsertInsertBranchesStampNow(t *testing.T) {
	st := openStore(t)
	mustNS(t, st, "test")
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "jobs", []schema.Field{
		{Name: "sku", Type: schema.String},
		{Name: "updated_at", Type: schema.Timestamp, Default: schema.NowDefault},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, _, _, err := st.UpsertByKey(ctx, "test", "jobs", []string{"sku"},
		[]map[string]any{{"sku": "a"}}, Embedder{}); err != nil {
		t.Fatalf("upsert_by_key insert branch: %v", err)
	}
	if _, err := st.Upsert(ctx, "test", "jobs", "sku = 'b'",
		nil, map[string]any{"sku": "b"}, Embedder{}); err != nil {
		t.Fatalf("filter upsert insert branch: %v", err)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT sku, updated_at FROM jobs ORDER BY id", nil, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2: %v", len(rows), rows)
	}
	for _, row := range rows {
		mustParseNowStamp(t, row["updated_at"])
	}
	stamp1 := rows[0]["updated_at"].(string)
	stamp2 := rows[1]["updated_at"].(string)

	if _, _, _, err := st.UpsertByKey(ctx, "test", "jobs", []string{"sku"},
		[]map[string]any{{"sku": "a"}}, Embedder{}); err != nil {
		t.Fatalf("upsert_by_key match: %v", err)
	}
	if _, err := st.Upsert(ctx, "test", "jobs", "sku = 'b'",
		nil, map[string]any{"sku": "b2"}, Embedder{}); err != nil {
		t.Fatalf("filter upsert match: %v", err)
	}
	rows, _, err = st.Query(ctx, "test", "SELECT sku, updated_at FROM jobs ORDER BY id", nil, 0, 0)
	if err != nil {
		t.Fatalf("query after updates: %v", err)
	}
	if rows[0]["updated_at"] != stamp1 || rows[1]["updated_at"] != stamp2 {
		t.Fatalf("update branches must keep the stored stamp instead of re-stamping, got %v", rows)
	}
}
