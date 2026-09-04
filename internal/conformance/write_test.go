package conformance

import (
	"testing"
)

// Write semantics: idempotency replay, divergence rejection, durability
// across a store reopen, upsert_by_key convergence, and upsert filter
// match/no-match.
func TestWriteIdempotencyReplayAndDivergence(t *testing.T) {
	h := newHarness(t)
	h.seedTable("wr", "t", []map[string]any{
		{"name": "title", "type": "string"},
		{"name": "n", "type": "number"},
	})
	recs := []map[string]any{{"title": "once", "n": 1}}

	first := h.mustHTTP("insert", map[string]any{
		"namespace": "wr", "table": "t", "idempotency_key": "k1", "records": recs,
	})
	ids := first["ids"].([]any)
	if len(ids) != 1 {
		t.Fatalf("first insert ids %v", ids)
	}
	// The replayed flag rides every idempotent insert (false on the first).
	if first["replayed"] != false {
		t.Fatalf("first insert must report replayed false: %v", first)
	}

	// Replay with the same key and the same records: original ids, nothing
	// re-inserted, replayed true.
	replay := h.mustHTTP("insert", map[string]any{
		"namespace": "wr", "table": "t", "idempotency_key": "k1", "records": recs,
	})
	assertJSONEqual(t, "replay ids", replay["ids"], ids)
	if int64val(t, "replay inserted", replay["inserted"]) != 0 {
		t.Fatalf("replay must insert 0, got %v", replay["inserted"])
	}
	if replay["replayed"] != true {
		t.Fatalf("replay must report replayed true: %v", replay)
	}

	// Row count unchanged by the replay.
	data := h.mustHTTP("describe_table", map[string]any{"namespace": "wr", "table": "t"})
	if int64val(t, "row count", data["row_count"]) != 1 {
		t.Fatalf("replay must not add rows: %v", data["row_count"])
	}

	// Divergence: same key, different records → conflict, nothing written.
	status, body := h.httpCall("insert", map[string]any{
		"namespace": "wr", "table": "t", "idempotency_key": "k1",
		"records": []map[string]any{{"title": "different", "n": 2}},
	})
	if status != 400 {
		t.Fatalf("divergent replay status %d, want 400: %v", status, body)
	}
	errObj := envelopeOf(t, body)
	if errObj["code"] != "conflict" {
		t.Fatalf("divergent replay code %v, want conflict", errObj["code"])
	}

	// A different key with the same records is a plain second insert.
	second := h.mustHTTP("insert", map[string]any{
		"namespace": "wr", "table": "t", "idempotency_key": "k2", "records": recs,
	})
	if second["replayed"] != false {
		t.Fatalf("fresh key must not replay: %v", second)
	}
	data = h.mustHTTP("describe_table", map[string]any{"namespace": "wr", "table": "t"})
	if int64val(t, "row count", data["row_count"]) != 2 {
		t.Fatalf("fresh key must insert: %v", data["row_count"])
	}
}

// Durability: the idempotency side table and plain writes survive a full
// server restart on the same directory — a retry after the restart still
// replays the original ids instead of duplicating rows.
func TestWriteDurabilityAcrossReopen(t *testing.T) {
	h := newHarness(t)
	h.seedTable("wr", "t", []map[string]any{{"name": "title", "type": "string"}})

	plain := h.mustHTTP("insert", map[string]any{
		"namespace": "wr", "table": "t",
		"records": []map[string]any{{"title": "plain"}},
	})
	idem := h.mustHTTP("insert", map[string]any{
		"namespace": "wr", "table": "t", "idempotency_key": "durable",
		"records": []map[string]any{{"title": "idem"}},
	})
	wantIDs := idem["ids"].([]any)
	if len(wantIDs) != 1 {
		t.Fatalf("idempotent insert ids %v", wantIDs)
	}

	h.reopen()

	// Plain writes survived.
	rows := h.mustHTTP("query", map[string]any{
		"namespace": "wr", "sql": "SELECT id, title FROM t ORDER BY id",
	})["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("after reopen: %d rows, want 2: %v", len(rows), rows)
	}
	got := rows[0].(map[string]any)
	if int64val(t, "first id", got["id"]) != int64val(t, "plain id", plain["ids"].([]any)[0]) {
		t.Fatalf("ids shifted across reopen: %v", rows)
	}

	// The idempotent retry still replays the original ids.
	replay := h.mustHTTP("insert", map[string]any{
		"namespace": "wr", "table": "t", "idempotency_key": "durable",
		"records": []map[string]any{{"title": "idem"}},
	})
	assertJSONEqual(t, "post-reopen replay ids", replay["ids"], wantIDs)
	if replay["replayed"] != true {
		t.Fatalf("post-reopen retry must replay: %v", replay)
	}
	data := h.mustHTTP("describe_table", map[string]any{"namespace": "wr", "table": "t"})
	if int64val(t, "row count", data["row_count"]) != 2 {
		t.Fatalf("post-reopen replay must not add rows: %v", data["row_count"])
	}
}

// upsert_by_key convergence: repeated calls with the same records converge
// (no duplicates), matched rows take a partial update (unspecified fields
// keep their values), and within a batch later records update earlier ones.
func TestWriteUpsertByKeyConvergence(t *testing.T) {
	h := newHarness(t)
	h.seedTable("wr", "u", []map[string]any{
		{"name": "email", "type": "string"},
		{"name": "tier", "type": "string"},
		{"name": "seats", "type": "number"},
	})

	call := func(recs ...map[string]any) map[string]any {
		return h.mustHTTP("upsert_by_key", map[string]any{
			"namespace": "wr", "table": "u", "on": []string{"email"}, "records": recs,
		})
	}

	// First call inserts.
	out := call(map[string]any{"email": "a@x", "tier": "free", "seats": 1})
	if int64val(t, "inserted", out["inserted"]) != 1 || int64val(t, "updated", out["updated"]) != 0 {
		t.Fatalf("first upsert_by_key: %v", out)
	}
	// Second call with the same key converges: update, no duplicate row.
	out = call(map[string]any{"email": "a@x", "seats": 5}) // partial: tier untouched
	if int64val(t, "inserted", out["inserted"]) != 0 || int64val(t, "updated", out["updated"]) != 1 {
		t.Fatalf("converging upsert_by_key: %v", out)
	}

	row := h.mustHTTP("query", map[string]any{
		"namespace": "wr", "sql": "SELECT email, tier, seats FROM u",
	})["rows"].([]any)[0].(map[string]any)
	assertJSONEqual(t, "partial update keeps unspecified fields", row["tier"], "free")
	assertJSONEqual(t, "partial update sets given fields", row["seats"], float64(5))

	// Batch-internal convergence: two records with the same key in one call
	// produce one row, the later record winning.
	out = call(
		map[string]any{"email": "b@x", "tier": "trial"},
		map[string]any{"email": "b@x", "tier": "pro"},
	)
	if int64val(t, "batch inserted", out["inserted"]) != 1 || int64val(t, "batch updated", out["updated"]) != 1 {
		t.Fatalf("batch with internal convergence: %v", out)
	}
	data := h.mustHTTP("describe_table", map[string]any{"namespace": "wr", "table": "u"})
	if int64val(t, "row count", data["row_count"]) != 2 {
		t.Fatalf("batch must converge to 2 rows total, got %v", data["row_count"])
	}
	row = h.mustHTTP("query", map[string]any{
		"namespace": "wr", "sql": "SELECT tier FROM u WHERE email = 'b@x'",
	})["rows"].([]any)[0].(map[string]any)
	assertJSONEqual(t, "later batch record wins", row["tier"], "pro")

	// Missing or null key fields are rejected.
	status, body := h.httpCall("upsert_by_key", map[string]any{
		"namespace": "wr", "table": "u", "on": []string{"email"},
		"records": []map[string]any{{"tier": "none"}},
	})
	if status != 400 {
		t.Fatalf("missing key field: status %d, want 400: %v", status, body)
	}
}

// upsert: filter matches → update semantics; filter matches nothing → one
// insert, which must satisfy required fields.
func TestWriteUpsertFilterMatchAndInsert(t *testing.T) {
	h := newHarness(t)
	h.seedTable("wr", "s", []map[string]any{
		{"name": "slug", "type": "string", "required": true},
		{"name": "count", "type": "number"},
	})

	// No match → insert branch: normalized write shape (ids, inserted,
	// updated) with inserted counted as rows.
	out := h.mustHTTP("upsert", map[string]any{
		"namespace": "wr", "table": "s", "filter": "slug = 'home'",
		"set": map[string]any{"slug": "home", "count": 1},
	})
	ids := out["ids"].([]any)
	if len(ids) != 1 || int64val(t, "inserted", out["inserted"]) != 1 || int64val(t, "updated", out["updated"]) != 0 {
		t.Fatalf("insert branch: %v", out)
	}
	newID := int64val(t, "inserted id", ids[0])

	// Match → update branch: the same row id, no insert.
	out = h.mustHTTP("upsert", map[string]any{
		"namespace": "wr", "table": "s", "filter": "slug = ?",
		"args": []any{"home"},
		"set":  map[string]any{"count": 2},
	})
	ids = out["ids"].([]any)
	if len(ids) != 1 || int64val(t, "updated", out["updated"]) != 1 || int64val(t, "inserted", out["inserted"]) != 0 {
		t.Fatalf("update branch: %v", out)
	}
	if int64val(t, "updated row id", ids[0]) != newID {
		t.Fatalf("update branch must name the same row: %v", out)
	}

	data := h.mustHTTP("describe_table", map[string]any{"namespace": "wr", "table": "s"})
	if int64val(t, "row count", data["row_count"]) != 1 {
		t.Fatalf("upsert must converge on one row: %v", data["row_count"])
	}
	row := h.mustHTTP("query", map[string]any{
		"namespace": "wr", "sql": "SELECT id, slug, count FROM s",
	})["rows"].([]any)[0].(map[string]any)
	if int64val(t, "row id", row["id"]) != newID {
		t.Fatalf("upsert update must keep the original row id: %v", row)
	}
	assertJSONEqual(t, "upsert set value", row["count"], float64(2))

	// Insert branch must still satisfy required fields.
	status, body := h.httpCall("upsert", map[string]any{
		"namespace": "wr", "table": "s", "filter": "slug = 'missing'",
		"set": map[string]any{"count": 9}, // no slug
	})
	if status != 400 {
		t.Fatalf("insert branch without required field: status %d, want 400: %v", status, body)
	}
	errObj := envelopeOf(t, body)
	if errObj["code"] != "invalid_request" {
		t.Fatalf("required-field rejection code %v", errObj["code"])
	}
}

// Write semantics on the MCP transport: a retried idempotent insert over
// tools/call replays identically to the HTTP retry.
func TestWriteIdempotencyOverMCP(t *testing.T) {
	h := newHarness(t)
	h.seedTable("wrm", "t", []map[string]any{{"name": "title", "type": "string"}})
	args := map[string]any{
		"namespace": "wrm", "table": "t", "idempotency_key": "mcp-1",
		"records": []map[string]any{{"title": "via mcp"}},
	}

	first := h.mustMCP("insert", args)
	if first["replayed"] == true {
		t.Fatalf("first MCP insert must not replay: %v", first)
	}
	replay := h.mustMCP("insert", args)
	assertJSONEqual(t, "MCP replay ids", replay["ids"], first["ids"])
	if replay["replayed"] != true || int64val(t, "inserted", replay["inserted"]) != 0 {
		t.Fatalf("MCP replay: %v", replay)
	}
	data := h.mustMCP("describe_table", map[string]any{"namespace": "wrm", "table": "t"})
	if int64val(t, "row count", data["row_count"]) != 1 {
		t.Fatalf("MCP replay must not add rows: %v", data["row_count"])
	}
}
