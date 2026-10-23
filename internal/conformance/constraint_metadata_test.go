package conformance

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnenforcedConstraintMetadataTeachesAtBothTransports(t *testing.T) {
	sqliteStorageInternalsOnly(t)
	for _, tc := range []struct{ name, value string }{
		{"constraints", `[{"name":"slug_key","kind":"unique","fields":["slug"]}]`},
		{"accept_references", `true`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.mustHTTP("create_namespace", map[string]any{"namespace": "app"})
			h.mustHTTP("create_table", map[string]any{"namespace": "app", "table": "notes", "fields": []map[string]any{{"name": "slug", "type": "string"}}})
			h.mustHTTP("insert", map[string]any{"namespace": "app", "table": "notes", "records": []map[string]any{{"slug": "original"}}})
			db, err := openSQL(filepath.Join(h.dir, "app.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`UPDATE _dolmen_tables SET schema_json=json_set(schema_json,?,json(?)) WHERE name='notes'`, "$."+tc.name, tc.value); err != nil {
				t.Fatal(err)
			}
			for _, op := range []string{"describe_table", "insert", "drop_table"} {
				body := map[string]any{"namespace": "app", "table": "notes"}
				if op == "insert" {
					body["records"] = []map[string]any{{"slug": "must_not_write"}}
				}
				status, result := h.httpCall(op, body)
				errEnv, _ := result["error"].(map[string]any)
				message, _ := errEnv["message"].(string)
				if status != http.StatusBadRequest || errEnv["code"] != "invalid_request" || !strings.Contains(message, "requires constraint enforcement") {
					t.Fatalf("HTTP %s did not teach the repair: %d %v", op, status, result)
				}
				mcpErr := h.mcpCall(op, body).toolError()
				if mcpErr == nil || mcpErr["code"] != "invalid_request" || mcpErr["message"] != message {
					t.Fatalf("MCP %s did not match HTTP refusal: %v", op, mcpErr)
				}
			}
			var count int
			if err := db.QueryRow(`SELECT count(*) FROM notes`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("refused operations changed rows: %d, %v", count, err)
			}
		})
	}
}
