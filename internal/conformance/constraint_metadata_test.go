package conformance

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoredSQLiteConstraintsAreEnforcedAtBothTransports(t *testing.T) {
	sqliteStorageInternalsOnly(t)
	h := newHarness(t)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "app"})
	h.mustHTTP("create_table", map[string]any{"namespace": "app", "table": "notes", "fields": []map[string]any{{"name": "slug", "type": "string"}}})
	h.mustHTTP("insert", map[string]any{"namespace": "app", "table": "notes", "records": []map[string]any{{"slug": "original"}}})
	db, err := openSQL(filepath.Join(h.dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE _dolmen_tables SET schema_json=json_set(schema_json,'$.constraints',json(?)) WHERE name='notes'`, `[{"name":"slug_key","kind":"unique","fields":["slug"]}]`); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"namespace": "app", "table": "notes", "records": []map[string]any{{"slug": "original"}}}
	status, result := h.httpCall("insert", body)
	errEnv, _ := result["error"].(map[string]any)
	message, _ := errEnv["message"].(string)
	if status != http.StatusBadRequest || errEnv["code"] != "conflict" || !strings.Contains(message, `constraint "slug_key"`) || strings.Contains(message, "original") {
		t.Fatalf("HTTP duplicate must be a conflict naming the constraint and not the value: %d %v", status, result)
	}
	mcpErr := h.mcpCall("insert", body).toolError()
	if mcpErr == nil || mcpErr["code"] != "conflict" || mcpErr["message"] != message {
		t.Fatalf("MCP duplicate did not match HTTP: %v", mcpErr)
	}
	h.mustHTTP("insert", map[string]any{"namespace": "app", "table": "notes", "records": []map[string]any{{"slug": "fresh"}}})
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM notes`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("refused duplicates changed rows: %d, %v", count, err)
	}
}
