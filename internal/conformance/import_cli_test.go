package conformance

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportCLIProducesTransportReadableExactRows(t *testing.T) {
	h := newHarness(t)
	h.seedTable("imp", "events", []map[string]any{{"name": "body", "type": "text"}})
	binary := filepath.Join(t.TempDir(), "dolmen")
	if out, err := exec.Command("go", "build", "-o", binary, "../../cmd/dolmen").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	for _, snapshot := range []string{"snapshot", "snapshot", "new-key"} {
		cmd := exec.Command(binary, "import", "-url", h.srv.URL, "-namespace", "imp", "-table", "events", "-snapshot", snapshot, "-rows", "1")
		cmd.Env = []string{}
		cmd.Stdin = strings.NewReader("{\"id\":9007199254740993,\"created_at\":\"2026-01-02T03:04:05.123Z\",\"body\":null}\n")
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "committed chunk 1") {
			t.Fatalf("CLI import: %v %s", err, out)
		}
	}
	rows := h.mustHTTPNumbered("read_rows", map[string]any{"namespace": "imp", "table": "events"})["rows"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["id"] != json.Number("9007199254740993") || rows[0].(map[string]any)["body"] != nil {
		t.Fatalf("readable rows: %v", rows)
	}
	result := h.mustMCP("query", map[string]any{"namespace": "imp", "sql": "SELECT count(*) AS n FROM events"})
	if result["rows"].([]any)[0].(map[string]any)["n"] != float64(1) {
		t.Fatalf("MCP count: %v", result)
	}
	cmd := exec.Command(binary, "import", "-url", h.srv.URL, "-namespace", "imp", "-table", "events", "-snapshot", "invalid-field", "-rows", "1")
	cmd.Env = []string{}
	cmd.Stdin = strings.NewReader(`{"id":50,"created_at":"2026-01-02T03:04:05.123Z","body":"retained"}` + "\n" +
		`{"id":51,"created_at":"2026-01-02T03:04:05.123Z","body":{"value":"NEVER-ECHO"}}` + "\n" +
		`{"id":52,"created_at":"2026-01-02T03:04:05.123Z","body":"not reached"}` + "\n")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "line 2 field body: invalid_request") || strings.Contains(string(out), "NEVER-ECHO") || strings.Contains(string(out), "committed chunk 2") {
		t.Fatalf("real coercion error: %v %s", err, out)
	}
	remaining := h.mustHTTPNumbered("query", map[string]any{"namespace": "imp", "sql": "SELECT id FROM events WHERE id IN (50,51,52) ORDER BY id"})["rows"].([]any)
	if len(remaining) != 1 || remaining[0].(map[string]any)["id"] != json.Number("50") {
		t.Fatalf("error must retain earlier chunks and stop: %v", remaining)
	}
}
