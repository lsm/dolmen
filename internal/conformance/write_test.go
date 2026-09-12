package conformance

import (
	"testing"
	"time"
)

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

	if first["replayed"] != false {
		t.Fatalf("first insert must report replayed false: %v", first)
	}

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

	data := h.mustHTTP("describe_table", map[string]any{"namespace": "wr", "table": "t"})
	if int64val(t, "row count", data["row_count"]) != 1 {
		t.Fatalf("replay must not add rows: %v", data["row_count"])
	}

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

	out := call(map[string]any{"email": "a@x", "tier": "free", "seats": 1})
	if int64val(t, "inserted", out["inserted"]) != 1 || int64val(t, "updated", out["updated"]) != 0 {
		t.Fatalf("first upsert_by_key: %v", out)
	}

	out = call(map[string]any{"email": "a@x", "seats": 5})
	if int64val(t, "inserted", out["inserted"]) != 0 || int64val(t, "updated", out["updated"]) != 1 {
		t.Fatalf("converging upsert_by_key: %v", out)
	}

	row := h.mustHTTP("query", map[string]any{
		"namespace": "wr", "sql": "SELECT email, tier, seats FROM u",
	})["rows"].([]any)[0].(map[string]any)
	assertJSONEqual(t, "partial update keeps unspecified fields", row["tier"], "free")
	assertJSONEqual(t, "partial update sets given fields", row["seats"], float64(5))

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

	for name, record := range map[string]map[string]any{
		"omitted": {"tier": "none"},
		"null":    {"email": nil, "tier": "none"},
	} {
		status, body := h.httpCall("upsert_by_key", map[string]any{
			"namespace": "wr", "table": "u", "on": []string{"email"},
			"records": []map[string]any{record},
		})
		if status != 400 {
			t.Fatalf("%s key field: status %d, want 400: %v", name, status, body)
		}
		errObj := envelopeOf(t, body)
		if errObj["code"] != "invalid_request" {
			t.Fatalf("%s key field code %v, want invalid_request", name, errObj["code"])
		}
	}
}

func TestWriteUpsertFilterMatchAndInsert(t *testing.T) {
	h := newHarness(t)
	h.seedTable("wr", "s", []map[string]any{
		{"name": "slug", "type": "string", "required": true},
		{"name": "count", "type": "number"},
	})

	out := h.mustHTTP("upsert", map[string]any{
		"namespace": "wr", "table": "s", "filter": "slug = 'home'",
		"set": map[string]any{"slug": "home", "count": 1},
	})
	ids := out["ids"].([]any)
	if len(ids) != 1 || int64val(t, "inserted", out["inserted"]) != 1 || int64val(t, "updated", out["updated"]) != 0 {
		t.Fatalf("insert branch: %v", out)
	}
	newID := int64val(t, "inserted id", ids[0])

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

	status, body := h.httpCall("upsert", map[string]any{
		"namespace": "wr", "table": "s", "filter": "slug = 'missing'",
		"set": map[string]any{"count": 9},
	})
	if status != 400 {
		t.Fatalf("insert branch without required field: status %d, want 400: %v", status, body)
	}
	errObj := envelopeOf(t, body)
	if errObj["code"] != "invalid_request" {
		t.Fatalf("required-field rejection code %v", errObj["code"])
	}
}

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

func TestWriteTimestampNowDefault(t *testing.T) {
	h := newHarness(t)
	ns := "wrnow"
	h.seedTable(ns, "t", []map[string]any{
		{"name": "title", "type": "string"},
		{"name": "updated_at", "type": "timestamp", "default": "now()"},
	})

	describe := h.mustHTTP("describe_table", map[string]any{"namespace": ns, "table": "t"})
	var declared any
	for _, f := range describe["table"].(map[string]any)["fields"].([]any) {
		fm := f.(map[string]any)
		if fm["name"] == "updated_at" {
			declared = fm["default"]
		}
	}
	if declared != "now()" {
		t.Fatalf("describe_table must report the declared now() default, got %v", declared)
	}

	readStamp := func(title string) any {
		data := h.mustHTTP("query", map[string]any{
			"namespace": ns, "sql": "SELECT title, updated_at FROM t",
		})
		for _, r := range data["rows"].([]any) {
			rm := r.(map[string]any)
			if rm["title"] == title {
				return rm["updated_at"]
			}
		}
		t.Fatalf("no row with title %q", title)
		return nil
	}

	before := time.Now()
	recs := []map[string]any{{"title": "once"}}
	first := h.mustHTTP("insert", map[string]any{
		"namespace": ns, "table": "t", "idempotency_key": "k1", "records": recs,
	})
	after := time.Now()
	stamp, ok := readStamp("once").(string)
	if !ok {
		t.Fatalf("omitted field must be stamped, got %v", readStamp("once"))
	}
	stamped, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatalf("stamp %q must be an RFC3339 timestamp: %v", stamp, err)
	}
	if stamped.Before(before.Add(-2*time.Second)) || stamped.After(after.Add(2*time.Second)) {
		t.Fatalf("omitted field must carry the server's write-time stamp, got %v", stamp)
	}

	replay := h.mustHTTP("insert", map[string]any{
		"namespace": ns, "table": "t", "idempotency_key": "k1", "records": recs,
	})
	assertJSONEqual(t, "replay ids", replay["ids"], first["ids"])
	if replay["replayed"] != true {
		t.Fatalf("omitted-field retry must replay, got %v", replay)
	}
	if got := readStamp("once"); got != stamp {
		t.Fatalf("replay must keep the original stamp, got %v want %v", got, stamp)
	}
	data := h.mustHTTP("describe_table", map[string]any{"namespace": ns, "table": "t"})
	if int64val(t, "row count", data["row_count"]) != 1 {
		t.Fatalf("replay must not add rows: %v", data["row_count"])
	}

	h.mustHTTP("insert", map[string]any{
		"namespace": ns, "table": "t",
		"records": []map[string]any{{"title": "supplied", "updated_at": "2026-01-02T03:04:05Z"}},
	})
	if got := readStamp("supplied"); got != "2026-01-02T03:04:05Z" {
		t.Fatalf("supplied timestamp must win over the now() default, got %v", got)
	}
}
