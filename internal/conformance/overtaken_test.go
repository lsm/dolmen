package conformance

import (
	"fmt"
	"net/http"
	"testing"
)

func seedOvertakenTable(t *testing.T, h *harness, ns string, rows int) {
	t.Helper()
	h.seedTable(ns, "docs", []map[string]any{{"name": "body", "type": "text"}})
	records := make([]map[string]any, 0, rows)
	for i := 0; i < rows; i++ {
		records = append(records, map[string]any{"body": fmt.Sprintf("row %d of the overtaken fixture", i)})
	}
	for start := 0; start < len(records); start += 100 {
		end := min(start+100, len(records))
		h.mustHTTP("insert", map[string]any{"namespace": ns, "table": "docs", "records": records[start:end]})
	}
}

func TestAMigrateOvertakenByAnotherMigrationReplansAndLands(t *testing.T) {
	const rows = 300
	h := newHarness(t)
	seedOvertakenTable(t, h, "overtaken", rows)

	var nestedStatus int
	var nestedBody map[string]any
	h.emb.onFirstCall = func() {
		nestedStatus, nestedBody = h.httpCall("migrate", map[string]any{
			"namespace": "overtaken", "table": "docs", "expected_version": 1,
			"changes": []map[string]any{{"op": "add_field", "field": map[string]any{"name": "note", "type": "string"}}},
		})
	}

	out := h.mustHTTP("migrate", map[string]any{
		"namespace": "overtaken", "table": "docs", "changes": vectorizeBody(),
	})
	if nestedStatus != http.StatusOK {
		t.Fatalf("the migration that landed mid-backfill answered %d: %v", nestedStatus, nestedBody)
	}
	table := out["table"].(map[string]any)
	if int64val(t, "version", table["version"]) != 3 {
		t.Fatalf("the overtaken migration left the table at version %v, want 3: the concurrent add_field took it to 2 and the re-planned vectorize is its own version, so both changes are recorded", table["version"])
	}
	fields := table["fields"].([]any)
	names := map[string]bool{}
	vectorized := false
	for _, f := range fields {
		field := f.(map[string]any)
		names[field["name"].(string)] = true
		if v, ok := field["vectorize"].(bool); ok && v {
			vectorized = true
		}
	}
	if !names["note"] {
		t.Fatalf("the landed schema has no note field, so the concurrent migration was lost: %v", fields)
	}
	if !vectorized {
		t.Fatalf("the landed schema does not vectorize, so the overtaken migration was lost: %v", fields)
	}
	hits := h.mustHTTP("search_vector", map[string]any{
		"namespace": "overtaken", "table": "docs", "text": "row 0 of the overtaken fixture", "limit": 1,
	})
	if skipped := int64val(t, "skipped_vectors", hits["skipped_vectors"]); skipped != 0 {
		t.Fatalf("%d rows hold no vector after the overtaken migration re-planned, so it applied without embedding them", skipped)
	}
}

func TestAMigrateWhoseTableIsReplacedIsRefused(t *testing.T) {
	h := newHarness(t)
	seedOvertakenTable(t, h, "replaced", 300)

	var nested []string
	h.emb.onFirstCall = func() {
		status, body := h.httpCall("drop_table", map[string]any{"namespace": "replaced", "table": "docs", "confirm": "docs"})
		nested = append(nested, fmt.Sprintf("drop %d", status))
		if status != http.StatusOK {
			t.Errorf("the mid-backfill drop_table answered %d: %v", status, body)
			return
		}
		h.mustHTTP("create_table", map[string]any{
			"namespace": "replaced", "table": "docs",
			"fields": []map[string]any{{"name": "body", "type": "text"}},
		})
		h.mustHTTP("insert", map[string]any{
			"namespace": "replaced", "table": "docs",
			"records": []map[string]any{{"body": "a row of the successor table"}},
		})
		nested = append(nested, "created")
	}

	status, body := h.httpCall("migrate", map[string]any{
		"namespace": "replaced", "table": "docs", "changes": vectorizeBody(),
	})
	if status != http.StatusNotFound {
		t.Fatalf("a migrate whose table was dropped and recreated answered %d, want 404: the successor is a different table and must not be migrated under the old migration's name (%v)", status, body)
	}
	if len(nested) != 2 {
		t.Fatalf("the mid-backfill replacement did not complete: %v", nested)
	}
	errObj := envelopeOf(t, body)
	wantMessage(t, "replaced table", errObj["message"].(string),
		`table replaced\.docs was replaced; describe the current table`)
	if errObj["code"] != "not_found" {
		t.Fatalf("a replaced table answered code %v, want not_found: this is not a version conflict, there is no version to argue about", errObj["code"])
	}
	sc := h.mustHTTP("describe_table", map[string]any{"namespace": "replaced", "table": "docs"})["table"].(map[string]any)
	if int64val(t, "version", sc["version"]) != 1 {
		t.Fatalf("the successor table is at version %v, want 1: the refused migration must not have vectorized it", sc["version"])
	}
	fields := sc["fields"].([]any)
	for _, f := range fields {
		field := f.(map[string]any)
		if v, ok := field["vectorize"].(bool); ok && v {
			t.Fatalf("the successor table is vectorized: the refused migration applied to it: %v", sc)
		}
	}
	status, body = h.httpCall("search_vector", map[string]any{
		"namespace": "replaced", "table": "docs", "text": "a row of the successor table", "limit": 1,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("a text search on the successor answered %d, want 400: it has no vectorize field, which is the other half of the refused migration not having touched it (%v)", status, body)
	}
}
