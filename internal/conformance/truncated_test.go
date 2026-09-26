package conformance

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestATruncatedNamespaceIsRefusedOnBothTransports(t *testing.T) {
	sqliteOnly(t)
	h := newHarness(t)
	h.seedTable("torn", "docs", []map[string]any{{"name": "body", "type": "string"}})
	h.seedTable("whole", "docs", []map[string]any{{"name": "body", "type": "string"}})
	h.mustHTTP("insert", map[string]any{"namespace": "whole", "table": "docs", "records": []map[string]any{{"body": "before"}}})

	h.srv.Close()
	if err := h.st.Close(); err != nil {
		t.Fatalf("close the store: %v", err)
	}
	path := filepath.Join(h.dir, "torn.db")
	if err := os.Truncate(path, 0); err != nil {
		t.Fatalf("truncate %s: %v", path, err)
	}
	h.reopen()

	status, out := h.httpCall("insert", map[string]any{"namespace": "torn", "table": "docs", "records": []map[string]any{{"body": "after"}}})
	if status != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500: a namespace whose file cannot be read is not the caller's mistake: %v", status, out)
	}
	env, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error envelope: %v", out)
	}
	if env["code"] != "internal_error" {
		t.Errorf("code %v, want internal_error: %v", env["code"], env)
	}
	msg, _ := env["message"].(string)
	for _, want := range []string{"namespace torn", "restore it from a backup", "unaffected"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message must say %q: %q", want, msg)
		}
	}
	if strings.Contains(msg, ".db") {
		t.Errorf("the message must not quote the file path: %q", msg)
	}
	for _, c := range []struct {
		op   string
		args map[string]any
	}{
		{"create_table", map[string]any{"namespace": "torn", "table": "again", "fields": []map[string]any{{"name": "body", "type": "string"}}}},
		{"insert", map[string]any{"namespace": "torn", "table": "docs", "records": []map[string]any{{"body": "after"}}}},
		{"query", map[string]any{"namespace": "torn", "sql": "SELECT 1"}},
		{"read_rows", map[string]any{"namespace": "torn", "table": "docs", "ids": []int{1}}},
		{"search_fulltext", map[string]any{"namespace": "torn", "table": "docs", "query": "after"}},
		{"describe_table", map[string]any{"namespace": "torn", "table": "docs"}},
	} {
		res := h.mcpCall(c.op, c.args)
		if !res.isError() {
			t.Errorf("MCP %s on a truncated namespace must fail: %+v", c.op, res)
			continue
		}
		env := res.toolError()
		if env["code"] != "internal_error" {
			t.Errorf("MCP %s code %v, want internal_error: %v", c.op, env["code"], env)
		}
		if m, _ := env["message"].(string); !strings.Contains(m, "namespace torn") {
			t.Errorf("MCP %s message must name the namespace: %q", c.op, m)
		}
	}
	if data := h.mustHTTP("insert", map[string]any{"namespace": "whole", "table": "docs", "records": []map[string]any{{"body": "after"}}}); data["inserted"] != float64(1) {
		t.Errorf("a healthy namespace must keep serving: %v", data)
	}
	rows := h.mustHTTP("read_rows", map[string]any{"namespace": "whole", "table": "docs", "ids": []int{1, 2}})
	if got, _ := rows["row_count"].(float64); got != 2 {
		t.Errorf("the healthy namespace lost a row: %v", rows)
	}
}
