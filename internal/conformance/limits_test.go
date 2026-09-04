package conformance

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

// The limits table test mirrors the README "Limits and guardrails" table,
// row by row. Every row is table-driven with its documented behavior:
// reject (4xx), clamp (silently bounded), or truncate (truncated:true).
// The constants are imported from the store/schema packages so a limit
// change must update both the code and this contract in one step.
func TestLimitsNamespaceName(t *testing.T) {
	h := newHarness(t)

	accept := []string{"a", "ab", "a1", "a-b", "a_b", strings.Repeat("n", 64), "9starts"}
	for _, ns := range accept {
		t.Run("accept "+shortName(ns), func(t *testing.T) {
			h.mustHTTP("create_namespace", map[string]any{"namespace": ns})
		})
	}
	reject := []struct {
		ns    string
		why   string
		msgRe string
	}{
		{strings.Repeat("n", 65), "65 chars", `namespace`},
		{"-leading", "leading dash", `namespace`},
		{"_leading", "leading underscore", `namespace`},
		{"with space", "space", `namespace`},
	}
	// Direct /v1 requests normalize names: trim + lowercase before validation.
	t.Run("uppercase normalizes to lowercase", func(t *testing.T) {
		data := h.mustHTTP("create_namespace", map[string]any{"namespace": " Upper "})
		if data["namespace"] != "upper" {
			t.Fatalf("normalized namespace %v, want \"upper\"", data["namespace"])
		}
	})
	for _, c := range reject {
		t.Run("reject "+c.why, func(t *testing.T) {
			status, body := h.httpCall("create_namespace", map[string]any{"namespace": c.ns})
			if status != 400 {
				t.Fatalf("status %d, want 400: %v", status, body)
			}
			errObj := envelopeOf(t, body)
			if errObj["code"] != "invalid_request" {
				t.Fatalf("code %v, want invalid_request", errObj["code"])
			}
		})
	}
}

func TestLimitsTableAndFieldNames(t *testing.T) {
	h := newHarness(t)
	base := func(table string, fields []map[string]any) (int, map[string]any) {
		return h.httpCall("create_table", map[string]any{
			"namespace": "lim", "table": table, "fields": fields,
		})
	}
	plain := []map[string]any{{"name": "a", "type": "string"}}

	// Accepted shapes: max-length names, digits after the first char.
	long := strings.Repeat("f", 64)
	if status, body := base("t"+strings.Repeat("x", 63), plain); status != 200 {
		t.Fatalf("64-char table name must be accepted: %d %v", status, body)
	}
	if status, body := base("fieldok", []map[string]any{{"name": long, "type": "string"}}); status != 200 {
		t.Fatalf("64-char field name must be accepted: %d %v", status, body)
	}

	rejected := []struct {
		what   string
		table  string
		fields []map[string]any
		msgRe  string
	}{
		{"reserved table id", "id", plain, `id`},
		{"sqlite_ table prefix", "sqlite_x", plain, `sqlite_`},
		{"__fts table", "x__ftsy", plain, `__fts`},
		{"65-char table", "t" + strings.Repeat("x", 64), plain, ""},
		{"65-char field", "fldlen", []map[string]any{{"name": strings.Repeat("f", 65), "type": "string"}}, ""},
		{"reserved field id", "fldres", []map[string]any{{"name": "id", "type": "string"}}, `record_id`},
		{"reserved field created_at", "fldres2", []map[string]any{{"name": "created_at", "type": "string"}}, `created_time`},
		{"reserved field _embedding", "fldres3", []map[string]any{{"name": "_embedding", "type": "string"}}, ""},
		{"reserved field _score", "fldres4", []map[string]any{{"name": "_score", "type": "string"}}, ""},
		{"reserved field _rank", "fldres5", []map[string]any{{"name": "_rank", "type": "string"}}, ""},
		{"reserved field rowid", "fldres6", []map[string]any{{"name": "rowid", "type": "string"}}, `record_id`},
		{"rank with fulltext", "fldrank", []map[string]any{{"name": "rank", "type": "string", "fulltext": true}}, `rank`},
		{"rank without fulltext ok is separate", "fldrank2", []map[string]any{{"name": "rank", "type": "string"}}, ""}, // accepted below
	}
	for _, c := range rejected {
		if strings.HasPrefix(c.what, "rank without") {
			continue
		}
		t.Run(c.what, func(t *testing.T) {
			status, body := base(c.table, c.fields)
			if status != 400 {
				t.Fatalf("status %d, want 400: %v", status, body)
			}
			errObj := envelopeOf(t, body)
			if errObj["code"] != "invalid_request" {
				t.Fatalf("code %v, want invalid_request", errObj["code"])
			}
			if c.msgRe != "" {
				wantMessage(t, c.what, errObj["message"].(string), c.msgRe)
			}
		})
	}
	// rank is only reserved for fulltext fields.
	if status, body := base("fldrank2", []map[string]any{{"name": "rank", "type": "string"}}); status != 200 {
		t.Fatalf("rank without fulltext must be accepted: %d %v", status, body)
	}
}

func TestLimitsFieldCount(t *testing.T) {
	h := newHarness(t)
	fields := func(n int) []map[string]any {
		out := make([]map[string]any, n)
		for i := range out {
			out[i] = map[string]any{"name": "f" + pad(i), "type": "string"}
		}
		return out
	}
	if status, body := h.httpCall("create_table", map[string]any{
		"namespace": "lim", "table": "maxf", "fields": fields(store.MaxFieldsPerTable),
	}); status != 200 {
		t.Fatalf("%d fields must be accepted: %d %v", store.MaxFieldsPerTable, status, body)
	}
	status, body := h.httpCall("create_table", map[string]any{
		"namespace": "lim", "table": "toomany", "fields": fields(store.MaxFieldsPerTable + 1),
	})
	if status != 400 {
		t.Fatalf("%d fields must be rejected, got %d", store.MaxFieldsPerTable+1, status)
	}
	errObj := envelopeOf(t, body)
	wantMessage(t, "field count", errObj["message"].(string),
		`too many fields: 101 \(max 100`)
}

func TestLimitsRecordsPerInsert(t *testing.T) {
	h := newHarness(t)
	h.seedTable("limrec", "t", []map[string]any{{"name": "a", "type": "string"}})

	recs := func(n int) []map[string]any {
		out := make([]map[string]any, n)
		for i := range out {
			out[i] = map[string]any{"a": "v" + pad(i)}
		}
		return out
	}

	// Exactly store.MaxRecordsPerInsert is accepted.
	data := h.mustHTTP("insert", map[string]any{
		"namespace": "limrec", "table": "t", "records": recs(store.MaxRecordsPerInsert),
	})
	if int64val(t, "inserted", data["inserted"]) != int64(store.MaxRecordsPerInsert) {
		t.Fatalf("inserted %v, want %d", data["inserted"], store.MaxRecordsPerInsert)
	}
	// One more is rejected, both for insert and upsert_by_key.
	for _, op := range []string{"insert", "upsert_by_key"} {
		body := map[string]any{
			"namespace": "limrec", "table": "t", "records": recs(store.MaxRecordsPerInsert + 1),
		}
		if op == "upsert_by_key" {
			body["on"] = []string{"a"}
		}
		status, out := h.httpCall(op, body)
		if status != 400 {
			t.Fatalf("%s with %d records: status %d, want 400", op, store.MaxRecordsPerInsert+1, status)
		}
		errObj := envelopeOf(t, out)
		wantMessage(t, op, errObj["message"].(string), `too many records: 1001 > 1000 per call`)
	}
}

func TestLimitsUpsertByKeyFields(t *testing.T) {
	h := newHarness(t)
	fields := []map[string]any{{"name": "k1", "type": "string"}}
	for i := 2; i <= store.MaxKeyFields+1; i++ {
		fields = append(fields, map[string]any{"name": "k" + string(rune('0'+i)), "type": "string"})
	}
	h.seedTable("limkey", "t", fields)

	rec := map[string]any{}
	keys := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			name := "k" + string(rune('1'+i))
			out[i] = name
			rec[name] = "v"
		}
		return out
	}

	// Exactly MaxKeyFields is accepted.
	h.mustHTTP("upsert_by_key", map[string]any{
		"namespace": "limkey", "table": "t", "on": keys(store.MaxKeyFields), "records": []map[string]any{rec},
	})
	// One more key field is rejected.
	status, body := h.httpCall("upsert_by_key", map[string]any{
		"namespace": "limkey", "table": "t", "on": keys(store.MaxKeyFields + 1), "records": []map[string]any{rec},
	})
	if status != 400 {
		t.Fatalf("9 key fields: status %d, want 400: %v", status, body)
	}
	errObj := envelopeOf(t, body)
	wantMessage(t, "key fields", errObj["message"].(string), `too many key fields: 9 > 8`)
}

func TestLimitsIdempotencyKeyLength(t *testing.T) {
	h := newHarness(t)
	h.seedTable("limkey", "t", []map[string]any{{"name": "a", "type": "string"}})
	rec := []map[string]any{{"a": "v"}}

	// 256 bytes accepted, 257 rejected (bytes, not characters).
	h.mustHTTP("insert", map[string]any{
		"namespace": "limkey", "table": "t", "idempotency_key": strings.Repeat("k", store.MaxIdempotencyKeyLen), "records": rec,
	})
	status, body := h.httpCall("insert", map[string]any{
		"namespace": "limkey", "table": "t", "idempotency_key": strings.Repeat("k", store.MaxIdempotencyKeyLen+1), "records": rec,
	})
	if status != 400 {
		t.Fatalf("257-byte key: status %d, want 400: %v", status, body)
	}
	errObj := envelopeOf(t, body)
	wantMessage(t, "key length", errObj["message"].(string), `idempotency key is 257 bytes \(max 256\)`)

	// Multi-byte characters count as bytes: 128 × 'é' = 256 bytes, fine;
	// 129 × 'é' = 258 bytes, rejected.
	h.mustHTTP("insert", map[string]any{
		"namespace": "limkey", "table": "t", "idempotency_key": strings.Repeat("é", 128), "records": rec,
	})
	status, body = h.httpCall("insert", map[string]any{
		"namespace": "limkey", "table": "t", "idempotency_key": strings.Repeat("é", 129), "records": rec,
	})
	if status != 400 {
		t.Fatalf("258-byte multi-byte key: status %d, want 400: %v", status, body)
	}

	// Empty key rejected — omitting the field is the non-idempotent path.
	status, body = h.httpCall("insert", map[string]any{
		"namespace": "limkey", "table": "t", "idempotency_key": "", "records": rec,
	})
	if status != 400 {
		t.Fatalf("empty key: status %d, want 400: %v", status, body)
	}
	wantMessage(t, "empty key", envelopeOf(t, body)["message"].(string), `must not be empty`)
}

func TestLimitsVectorDimension(t *testing.T) {
	h := newHarness(t)

	// The documented ceiling is 4096: pin the constant so raising it
	// requires updating this contract (and the README) explicitly, instead
	// of the accept/reject pair below silently following the change.
	if schema.MaxVectorDim != 4096 {
		t.Fatalf("schema.MaxVectorDim = %d, want the documented ceiling 4096", schema.MaxVectorDim)
	}

	for _, dim := range []int{1, schema.MaxVectorDim} {
		if status, body := h.httpCall("create_table", map[string]any{
			"namespace": "limdim", "table": "v" + pad(dim), "fields": []map[string]any{{"name": "v", "type": "vector", "dim": dim}},
		}); status != 200 {
			t.Fatalf("dim %d must be accepted: %d %v", dim, status, body)
		}
	}
	for _, dim := range []int{0, schema.MaxVectorDim + 1} {
		status, body := h.httpCall("create_table", map[string]any{
			"namespace": "limdim", "table": "bad" + pad(dim), "fields": []map[string]any{{"name": "v", "type": "vector", "dim": dim}},
		})
		if status != 400 {
			t.Fatalf("dim %d must be rejected, got %d: %v", dim, status, body)
		}
	}
}

func TestLimitsSearchLimitClampAndDefault(t *testing.T) {
	h := newHarness(t)
	h.seedTable("limsrc", "t", []map[string]any{
		{"name": "title", "type": "string", "fulltext": true},
		{"name": "v", "type": "vector", "dim": 2},
	})
	recs := make([]map[string]any, 250)
	for i := range recs {
		recs[i] = map[string]any{"title": "shared needle " + pad(i), "v": []any{1, 0}}
	}
	h.mustHTTP("insert", map[string]any{"namespace": "limsrc", "table": "t", "records": recs})

	cases := []struct {
		name       string
		limit      any
		wantResult int
	}{
		{"default 10 when omitted", nil, 10},
		{"zero selects default 10", 0, 10},
		{"negative selects default 10", -3, 10},
		{"max 200 accepted", 200, 200},
		{"above 200 clamps to 200", 500, 200},
	}
	// The two search operations have separate handlers; the limits row
	// covers both, so the matrix runs through each.
	searches := []struct {
		name  string
		op    string
		extra map[string]any
	}{
		{"fulltext", "search_fulltext", map[string]any{"query": "needle"}},
		{"vector", "search_vector", map[string]any{"column": "v", "vector": []any{1, 0}}},
	}
	for _, s := range searches {
		for _, c := range cases {
			t.Run(c.name+" "+s.name, func(t *testing.T) {
				body := map[string]any{"namespace": "limsrc", "table": "t"}
				for k, v := range s.extra {
					body[k] = v
				}
				if c.limit != nil {
					body["limit"] = c.limit
				}
				data := h.mustHTTP(s.op, body)
				results := data["results"].([]any)
				if len(results) != c.wantResult {
					t.Fatalf("got %d results, want %d", len(results), c.wantResult)
				}
				if data["truncated"] != (len(results) < 250) {
					t.Fatalf("truncated must be %v with %d of 250 matches", len(results) < 250, len(results))
				}
			})
		}
	}
}

func TestLimitsQueryRowsTruncate(t *testing.T) {
	h := newHarness(t)
	h.seedTable("limrows", "t", []map[string]any{{"name": "n", "type": "number"}})

	recs := func(from, to int) []map[string]any {
		out := make([]map[string]any, 0, to-from)
		for i := from; i < to; i++ {
			out = append(out, map[string]any{"n": i})
		}
		return out
	}
	// 1001 rows via two inserts (each under the 1000-record limit).
	h.mustHTTP("insert", map[string]any{"namespace": "limrows", "table": "t", "records": recs(0, 1000)})
	h.mustHTTP("insert", map[string]any{"namespace": "limrows", "table": "t", "records": recs(1000, 1001)})

	// Default limit: 1000 of 1001 rows, truncated true.
	data := h.mustHTTP("query", map[string]any{"namespace": "limrows", "sql": "SELECT n FROM t"})
	rows := data["rows"].([]any)
	if len(rows) != 1000 {
		t.Fatalf("got %d rows, want the documented 1000 cap", len(rows))
	}
	if data["truncated"] != true {
		t.Fatalf("truncated must be true past the cap, got %v", data["truncated"])
	}
	if int64val(t, "row_count", data["row_count"]) != 1000 {
		t.Fatalf("row_count %v", data["row_count"])
	}
	// An explicit limit above the cap clamps to the cap (1000 of 1001 rows,
	// truncated true) — not to the requested 1500.
	data = h.mustHTTP("query", map[string]any{
		"namespace": "limrows", "sql": "SELECT n FROM t", "limit": 1500,
	})
	if got := len(data["rows"].([]any)); got != 1000 {
		t.Fatalf("limit 1500 must clamp to 1000 rows, got %d", got)
	}
	if data["truncated"] != true {
		t.Fatalf("clamped over-cap limit must report truncated, got %v", data["truncated"])
	}
	// Explicit smaller limit truncates too.
	data = h.mustHTTP("query", map[string]any{"namespace": "limrows", "sql": "SELECT n FROM t", "limit": 10})
	if len(data["rows"].([]any)) != 10 || data["truncated"] != true {
		t.Fatalf("limit 10: got %d rows, truncated %v", len(data["rows"].([]any)), data["truncated"])
	}
	// Exactly at the boundary is not truncated.
	data = h.mustHTTP("query", map[string]any{
		"namespace": "limrows", "sql": "SELECT n FROM t WHERE n < 10",
	})
	if len(data["rows"].([]any)) != 10 || data["truncated"] != false {
		t.Fatalf("full result set: got %d rows, truncated %v", len(data["rows"].([]any)), data["truncated"])
	}
}

func TestLimitsQueryResponseBudget(t *testing.T) {
	h := newHarness(t)
	h.seedTable("limbudget", "t", []map[string]any{{"name": "blob", "type": "text"}})

	// The documented budget is 32 MiB: pin the constant so changing it
	// requires updating this contract explicitly.
	if store.MaxQueryBytes != 32<<20 {
		t.Fatalf("store.MaxQueryBytes = %d, want the documented 32 MiB budget", store.MaxQueryBytes)
	}

	// 40 rows of exactly 1 MiB, inserted one row per call so each request
	// stays under the 32 MiB request-body limit.
	big := strings.Repeat("x", 1<<20)
	for i := 0; i < 40; i++ {
		h.mustHTTP("insert", map[string]any{
			"namespace": "limbudget", "table": "t", "records": []map[string]any{{"blob": big}},
		})
	}

	// Each row costs its 1 MiB text plus label/encoding overhead (~20
	// bytes), so exactly 31 rows fit under the 32 MiB budget: 31 × (1 MiB +
	// ε) ≈ 31.0 MiB fits; the 32nd would push past 33554432 bytes. Pinning
	// the count verifies both sides of the boundary — a budget shrunk to
	// ~28 MiB returns ≤ 28 rows, one grown to ~39 MiB returns more — instead
	// of just "fewer than 40".
	data := h.mustHTTP("query", map[string]any{"namespace": "limbudget", "sql": "SELECT blob FROM t"})
	rows := data["rows"].([]any)
	if len(rows) != 31 {
		t.Fatalf("the 32 MiB budget must fit exactly 31 one-MiB rows, got %d", len(rows))
	}
	if data["truncated"] != true {
		t.Fatalf("truncated must be true when the budget cuts the page, got %v", data["truncated"])
	}

	// The returned payload itself must sit just under the budget.
	total := 0
	for _, r := range rows {
		raw, _ := json.Marshal(r)
		total += len(raw)
	}
	if total < 31<<20 || total > 32<<20 {
		t.Fatalf("returned payload %d bytes must sit just under the 32 MiB budget", total)
	}
}

// The search ops enforce the same 32 MiB budget through their own row-fetch
// path (fetchByIDs in internal/store/search.go), separate from query's — so
// a regression there would leave the query-boundary test above green. Pin
// the same boundary for both search operations.
func TestLimitsSearchResponseBudget(t *testing.T) {
	h := newHarness(t)
	h.seedTable("limsearch", "f", []map[string]any{
		{"name": "title", "type": "string", "fulltext": true},
		{"name": "body", "type": "text", "vectorize": true},
		{"name": "blob", "type": "text"},
	})
	// 40 rows of exactly 1 MiB, eight records per insert so each request
	// stays under the 32 MiB request-body limit.
	big := strings.Repeat("x", 1<<20)
	for i := 0; i < 40; i += 8 {
		recs := make([]map[string]any, 0, 8)
		for j := 0; j < 8 && i+j < 40; j++ {
			recs = append(recs, map[string]any{"title": "needle", "body": "blob needle", "blob": big})
		}
		h.mustHTTP("insert", map[string]any{"namespace": "limsearch", "table": "f", "records": recs})
	}

	// limit 200 lifts the row-count cap so only the byte budget can cut
	// the page; like query, 31 one-MiB rows fit under the 32 MiB budget.
	fts := h.mustHTTP("search_fulltext", map[string]any{
		"namespace": "limsearch", "table": "f", "query": "needle", "limit": 200,
	})
	if got := len(fts["results"].([]any)); got != 31 {
		t.Fatalf("fulltext budget must cut at 31 one-MiB rows, got %d", got)
	}
	if fts["truncated"] != true {
		t.Fatalf("fulltext budget cut must set truncated, got %v", fts["truncated"])
	}

	sv := h.mustHTTP("search_vector", map[string]any{
		"namespace": "limsearch", "table": "f", "text": "blob needle", "limit": 200,
	})
	if got := len(sv["results"].([]any)); got != 31 {
		t.Fatalf("vector budget must cut at 31 one-MiB rows, got %d", got)
	}
	if sv["truncated"] != true {
		t.Fatalf("vector budget cut must set truncated, got %v", sv["truncated"])
	}
	// Every row scored — the cut is pure response budget, not skipped data.
	if int64val(t, "skipped", sv["skipped_vectors"]) != 0 {
		t.Fatalf("skipped_vectors %v, want 0", sv["skipped_vectors"])
	}
}

// The first-row-over-budget branches are errors, not truncated pages: with no
// prior row to cut after, the contract is a 400 naming the budget. Query can
// exceed the budget through one row whose selected columns alone (here, many
// aliases of one 1 MiB column) cross it; the documented oversized-BLOB case
// (a stored BLOB over 32 MiB always errors) can only exist via an out-of-band
// writer — the same fixture pattern as skipped_vectors — and applies to all
// three typed read paths.
func TestLimitsResponseBudgetFirstRowError(t *testing.T) {
	h := newHarness(t)
	h.seedTable("limalias", "t", []map[string]any{{"name": "meg", "type": "text"}})
	h.mustHTTP("insert", map[string]any{
		"namespace": "limalias", "table": "t",
		"records": []map[string]any{{"meg": strings.Repeat("x", 1<<20)}},
	})

	// 40 distinct aliases of the one-MiB column charge ~40 MiB for a single
	// row: an error on the first row, not an empty truncated page.
	cols := make([]string, 40)
	for i := range cols {
		cols[i] = fmt.Sprintf("meg AS m%02d", i)
	}
	status, body := h.httpCall("query", map[string]any{
		"namespace": "limalias", "sql": "SELECT " + strings.Join(cols, ", ") + " FROM t",
	})
	if status != 400 {
		t.Fatalf("first row over budget: status %d, want 400: %v", status, body)
	}
	errObj := envelopeOf(t, body)
	if errObj["code"] != "invalid_request" {
		t.Fatalf("first-row budget code %v, want invalid_request", errObj["code"])
	}
	wantMessage(t, "first-row budget", errObj["message"].(string),
		`query result exceeds the 32 MiB response budget on its first row`)

	// The documented oversized BLOB: stored over 32 MiB by an out-of-band
	// writer (the API's own 32 MiB request limit keeps honest rows under
	// it), it errors on every typed read path that would return the row.
	h.seedTable("limblob", "t", []map[string]any{
		{"name": "title", "type": "string", "fulltext": true},
		{"name": "body", "type": "text", "vectorize": true},
		{"name": "blob", "type": "text"},
	})
	h.mustHTTP("insert", map[string]any{
		"namespace": "limblob", "table": "t",
		"records": []map[string]any{{"title": "needle", "body": "needle text", "blob": "x"}},
	})
	h.outOfBand("limblob", func(db *sqlDB) error {
		_, err := db.Exec("UPDATE t SET blob = ? WHERE id = 1", make([]byte, 33<<20))
		return err
	})

	oversized := []struct {
		name string
		op   string
		body map[string]any
	}{
		{"query", "query", map[string]any{"namespace": "limblob", "sql": "SELECT blob FROM t"}},
		{"search_fulltext", "search_fulltext", map[string]any{
			"namespace": "limblob", "table": "t", "query": "needle",
		}},
		{"search_vector", "search_vector", map[string]any{
			"namespace": "limblob", "table": "t", "text": "needle text",
		}},
	}
	for _, c := range oversized {
		t.Run(c.name+" oversized blob errors", func(t *testing.T) {
			status, body := h.httpCall(c.op, c.body)
			if status != 400 {
				t.Fatalf("status %d, want 400: %v", status, body)
			}
			errObj := envelopeOf(t, body)
			if errObj["code"] != "invalid_request" {
				t.Fatalf("code %v, want invalid_request: %v", errObj["code"], errObj)
			}
			wantMessage(t, c.name, errObj["message"].(string),
				`column "blob" exceeds the 32 MiB response budget`)
		})
	}
}

func TestLimitsQueryArgs(t *testing.T) {
	h := newHarness(t)
	h.seedTable("limargs", "t", []map[string]any{{"name": "n", "type": "number"}})

	args := func(n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = float64(i)
		}
		return out
	}
	// 100 placeholders accepted: c0 = ? OR c1 = ? OR ... a 100-clause filter.
	ph := make([]string, 100)
	for i := range ph {
		ph[i] = "n = ?"
	}
	h.mustHTTP("query", map[string]any{
		"namespace": "limargs", "sql": "SELECT n FROM t WHERE " + strings.Join(ph, " OR "), "args": args(100),
	})
	// 101 args rejected.
	status, body := h.httpCall("query", map[string]any{
		"namespace": "limargs", "sql": "SELECT n FROM t WHERE n IN (1)", "args": args(101),
	})
	if status != 400 {
		t.Fatalf("101 args: status %d, want 400: %v", status, body)
	}
	wantMessage(t, "query args", envelopeOf(t, body)["message"].(string), `at most 100|too many`)
}

func TestLimitsInferSchemaSamples(t *testing.T) {
	h := newHarness(t)

	samples := func(n int) []map[string]any {
		out := make([]map[string]any, n)
		for i := range out {
			out[i] = map[string]any{"a": i}
		}
		return out
	}
	h.mustHTTP("infer_schema", map[string]any{"samples": samples(50)}) // 50 accepted
	status, body := h.httpCall("infer_schema", map[string]any{"samples": samples(51)})
	if status != 400 {
		t.Fatalf("51 samples: status %d, want 400: %v", status, body)
	}
	wantMessage(t, "samples", envelopeOf(t, body)["message"].(string), `too many samples: 51 > 50`)
	status, body = h.httpCall("infer_schema", map[string]any{"samples": samples(0)})
	if status != 400 {
		t.Fatalf("0 samples: status %d, want 400: %v", status, body)
	}
	wantMessage(t, "samples", envelopeOf(t, body)["message"].(string), `samples must not be empty`)
}

func TestLimitsColumnLabelLength(t *testing.T) {
	h := newHarness(t)
	h.seedTable("limlabel", "t", []map[string]any{{"name": "n", "type": "number"}})

	long := strings.Repeat("l", 4097)
	status, body := h.httpCall("query", map[string]any{
		"namespace": "limlabel", "sql": `SELECT n AS "` + long + `" FROM t`,
	})
	if status != 400 {
		t.Fatalf("4097-byte label: status %d, want 400: %v", status, body)
	}
	// A 4096-byte label is the documented ceiling.
	h.mustHTTP("query", map[string]any{
		"namespace": "limlabel", "sql": `SELECT n AS "` + strings.Repeat("l", 4096) + `" FROM t`,
	})
}

// envelopeOf extracts the error object from a decoded HTTP body.
func envelopeOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error envelope, got %v", body)
	}
	return errObj
}

// pad renders i as a zero-padded 4-digit string so generated names sort
// lexicographically in insertion order.
func pad(i int) string { return fmt.Sprintf("%04d", i) }

// shortName truncates long generated names for subtest labels.
func shortName(s string) string {
	if len(s) > 12 {
		return s[:8] + "…"
	}
	return s
}
