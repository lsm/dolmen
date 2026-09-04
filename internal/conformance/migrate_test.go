package conformance

import (
	"testing"
)

// Migration guards: expected_version preconditions, dry_run purity (no
// writes, no provider calls), and the list_migrations audit trail.
func TestMigrateExpectedVersionGuard(t *testing.T) {
	h := newHarness(t)
	h.seedTable("mig", "t", []map[string]any{
		{"name": "title", "type": "string"},
		{"name": "body", "type": "text"},
	})

	// Correct expected_version applies.
	out := h.mustHTTP("migrate", map[string]any{
		"namespace": "mig", "table": "t", "expected_version": 1,
		"changes": []map[string]any{{"op": "set_fulltext", "name": "title", "value": true}},
	})
	if table := out["table"].(map[string]any); int64val(t, "version", table["version"]) != 2 {
		t.Fatalf("version after migrate: %v", table["version"])
	}

	// Stale expected_version conflicts with 409 and the documented shape.
	status, body := h.httpCall("migrate", map[string]any{
		"namespace": "mig", "table": "t", "expected_version": 1,
		"changes": []map[string]any{{"op": "set_fulltext", "name": "body", "value": true}},
	})
	if status != 409 {
		t.Fatalf("stale version: status %d, want 409: %v", status, body)
	}
	errObj := envelopeOf(t, body)
	if errObj["code"] != "conflict" {
		t.Fatalf("stale version code %v, want conflict", errObj["code"])
	}
	wantMessage(t, "version conflict", errObj["message"].(string),
		`version conflict on mig\.t: schema is at version 2, expected 1`)

	// A current expected_version applies again.
	h.mustHTTP("migrate", map[string]any{
		"namespace": "mig", "table": "t", "expected_version": 2,
		"changes": []map[string]any{{"op": "set_fulltext", "name": "body", "value": true}},
	})

	// Destructive ops require expected_version outright.
	for _, changes := range [][]map[string]any{
		{{"op": "rename_field", "from": "body", "to": "content"}},
		{{"op": "drop_field", "name": "body"}},
	} {
		status, body := h.httpCall("migrate", map[string]any{
			"namespace": "mig", "table": "t", "changes": changes,
		})
		if status != 400 {
			t.Fatalf("destructive change without expected_version: status %d, want 400: %v", status, body)
		}
		wantMessage(t, "destructive guard", envelopeOf(t, body)["message"].(string),
			`destructive changes require expected_version`)
	}

	// A conflict on a destructive op is the 409, not the missing-version 400.
	status, body = h.httpCall("migrate", map[string]any{
		"namespace": "mig", "table": "t", "expected_version": 1,
		"changes": []map[string]any{{"op": "drop_field", "name": "body"}},
	})
	if status != 409 {
		t.Fatalf("stale destructive: status %d, want 409: %v", status, body)
	}
}

// dry_run purity: a preview makes no writes (schema version, rows, migration
// history unchanged) and no embedding provider calls.
func TestMigrateDryRunPurity(t *testing.T) {
	h := newHarness(t)
	h.seedTable("mig2", "t", []map[string]any{
		{"name": "body", "type": "text"},
	})
	h.mustHTTP("insert", map[string]any{
		"namespace": "mig2", "table": "t",
		"records": []map[string]any{{"body": "seed one"}, {"body": "seed two"}},
	})
	before := h.mustHTTP("describe_table", map[string]any{"namespace": "mig2", "table": "t"})
	histBefore := h.mustHTTP("list_migrations", map[string]any{"namespace": "mig2", "table": "t"})
	embedsBefore := h.emb.callCount()

	plan := h.mustHTTP("migrate", map[string]any{
		"namespace": "mig2", "table": "t", "dry_run": true,
		"changes": []map[string]any{
			{"op": "add_field", "field": map[string]any{"name": "status", "type": "string"}, "default": "open"},
			{"op": "set_vectorize", "name": "body", "value": true},
		},
	})

	// The plan shape: dry_run true, from/to versions, prospective table, and
	// the reported row-level work.
	if plan["dry_run"] != true {
		t.Fatalf("plan must report dry_run true: %v", plan)
	}
	if _, ok := plan["plan"]; !ok {
		t.Fatalf("dry_run response must carry the plan object: %v", plan)
	}
	p := plan["plan"].(map[string]any)
	if int64val(t, "from_version", p["from_version"]) != 1 || int64val(t, "to_version", p["to_version"]) != 2 {
		t.Fatalf("plan versions: %v", p)
	}
	if int64val(t, "backfill_rows", p["backfill_rows"]) != 2 {
		t.Fatalf("both rows receive the added default: %v", p["backfill_rows"])
	}
	if int64val(t, "embed_rows", p["embed_rows"]) != 2 {
		t.Fatalf("enabling vectorize would embed both rows: %v", p["embed_rows"])
	}
	// Enabling vectorize for the first time clears nothing — the column was
	// empty. (Disabling an existing vectorize field is what clears.)
	if p["clears_embeddings"] != false {
		t.Fatalf("first-time set_vectorize must not claim cleared embeddings: %v", p["clears_embeddings"])
	}
	// The returned table is the prospective schema, with the new field.
	table := plan["table"].(map[string]any)
	if int64val(t, "prospective version", table["version"]) != 2 {
		t.Fatalf("prospective table must show the bumped version: %v", table["version"])
	}

	// Purity: nothing changed.
	after := h.mustHTTP("describe_table", map[string]any{"namespace": "mig2", "table": "t"})
	assertJSONEqual(t, "schema unchanged by dry_run", after, before)
	histAfter := h.mustHTTP("list_migrations", map[string]any{"namespace": "mig2", "table": "t"})
	assertJSONEqual(t, "history unchanged by dry_run", histAfter, histBefore)
	if got := h.emb.callCount(); got != embedsBefore {
		t.Fatalf("dry_run must not call the embedder: %d calls before, %d after", embedsBefore, got)
	}
	// The default was not backfilled.
	rows := h.mustHTTP("query", map[string]any{
		"namespace": "mig2", "sql": "SELECT * FROM t",
	})["rows"].([]any)
	if _, ok := rows[0].(map[string]any)["status"]; ok {
		t.Fatalf("dry_run must not add columns to rows: %v", rows[0])
	}
}

// The audit trail: every applied migration is recorded newest first with its
// exact changes; the newest to_version is the current schema version.
func TestMigrateAuditTrail(t *testing.T) {
	h := newHarness(t)
	h.seedTable("mig3", "t", []map[string]any{
		{"name": "title", "type": "string", "fulltext": true},
	})
	h.mustHTTP("insert", map[string]any{
		"namespace": "mig3", "table": "t",
		"records": []map[string]any{{"title": "seed"}},
	})

	// Creating the table is version 1 and predates the log.
	hist := h.mustHTTP("list_migrations", map[string]any{"namespace": "mig3", "table": "t"})
	if len(hist["migrations"].([]any)) != 0 {
		t.Fatalf("fresh table has no migration history: %v", hist)
	}

	h.mustHTTP("migrate", map[string]any{
		"namespace": "mig3", "table": "t", "expected_version": 1,
		"changes": []map[string]any{{
			"op": "add_field", "field": map[string]any{"name": "status", "type": "string"}, "default": "open",
		}},
	})
	h.mustHTTP("migrate", map[string]any{
		"namespace": "mig3", "table": "t", "expected_version": 2,
		"changes": []map[string]any{{"op": "rename_field", "from": "status", "to": "state"}},
	})

	hist = h.mustHTTP("list_migrations", map[string]any{"namespace": "mig3", "table": "t"})
	ms := hist["migrations"].([]any)
	if len(ms) != 2 {
		t.Fatalf("two migrations recorded, got %v", hist)
	}

	// Newest first.
	newest := ms[0].(map[string]any)
	if int64val(t, "newest from", newest["from_version"]) != 2 || int64val(t, "newest to", newest["to_version"]) != 3 {
		t.Fatalf("newest entry: %v", newest)
	}
	if at, ok := newest["at"].(string); !ok || !rfc3339ish(at) {
		t.Fatalf("migration timestamp must be RFC3339: %v", newest["at"])
	}
	// The recorded change is the exact input, replayable through migrate.
	change := newest["changes"].([]any)[0].(map[string]any)
	if change["op"] != "rename_field" || change["from"] != "status" || change["to"] != "state" {
		t.Fatalf("recorded rename: %v", change)
	}

	oldest := ms[1].(map[string]any)
	if int64val(t, "oldest from", oldest["from_version"]) != 1 || int64val(t, "oldest to", oldest["to_version"]) != 2 {
		t.Fatalf("oldest entry: %v", oldest)
	}
	added := oldest["changes"].([]any)[0].(map[string]any)
	if added["op"] != "add_field" || added["default"] != "open" {
		t.Fatalf("recorded add_field must keep its default: %v", added)
	}

	// The add_field default was backfilled into the existing row.
	row := h.mustHTTP("query", map[string]any{
		"namespace": "mig3", "sql": "SELECT state FROM t",
	})["rows"].([]any)[0].(map[string]any)
	assertJSONEqual(t, "backfilled default", row["state"], "open")

	// drop_table erases the history with the table.
	h.mustHTTP("drop_table", map[string]any{"namespace": "mig3", "table": "t", "confirm": "t"})
	h.seedTable("mig3", "t", []map[string]any{{"name": "title", "type": "string"}})
	hist = h.mustHTTP("list_migrations", map[string]any{"namespace": "mig3", "table": "t"})
	if len(hist["migrations"].([]any)) != 0 {
		t.Fatalf("recreated table starts with no history: %v", hist)
	}
}

// rfc3339ish checks the documented timestamp shape loosely (YYYY-MM-DDTHH:MM:SS...Z).
func rfc3339ish(s string) bool {
	if len(s) < 20 {
		return false
	}
	return s[4] == '-' && s[7] == '-' && s[10] == 'T' && s[13] == ':' && s[16] == ':'
}
