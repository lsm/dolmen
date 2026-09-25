package conformance

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/store"
)

func TestVacuumReportsSizesOnBothTransports(t *testing.T) {
	h := newHarness(t)
	h.seedTable("vac", "notes", []map[string]any{{"name": "body", "type": "text"}})
	body := strings.Repeat("x", 16<<10)
	records := make([]map[string]any, 32)
	for i := range records {
		records[i] = map[string]any{"body": body}
	}
	h.mustHTTP("insert", map[string]any{"namespace": "vac", "table": "notes", "records": records})
	h.mustHTTP("delete", map[string]any{"namespace": "vac", "table": "notes", "filter": "1=1", "limit": 1000, "confirm": true})

	httpOut := h.mustHTTP("vacuum", map[string]any{"namespace": "vac"})
	mcpOut := h.mustMCP("vacuum", map[string]any{"namespace": "vac"})
	for name, out := range map[string]map[string]any{"http": httpOut, "mcp": mcpOut} {
		if out["namespace"] != "vac" {
			t.Fatalf("%s: namespace %v", name, out["namespace"])
		}
		for _, k := range []string{"bytes_before", "bytes_after"} {
			n, ok := out[k].(json.Number)
			if !ok {
				f, isFloat := out[k].(float64)
				if !isFloat || f < 0 {
					t.Fatalf("%s: %s must be a non-negative integer, got %#v", name, k, out[k])
				}
				continue
			}
			if v, err := n.Int64(); err != nil || v < 0 {
				t.Fatalf("%s: %s must be a non-negative integer, got %v", name, k, n)
			}
		}
	}
	if activeEngine == store.EngineSQLite {
		before, _ := toFloat(httpOut["bytes_before"])
		after, _ := toFloat(httpOut["bytes_after"])
		if after >= before {
			t.Fatalf("vacuum after deleting every row must shrink the namespace file: before %v after %v", before, after)
		}
	}
	h.mustHTTP("insert", map[string]any{"namespace": "vac", "table": "notes", "records": []map[string]any{{"body": "again"}}})
}

func TestVacuumOfAMissingNamespaceIsNotFound(t *testing.T) {
	h := newHarness(t)
	status, out := h.httpCall("vacuum", map[string]any{"namespace": "nosuchns"})
	if status != 404 {
		t.Fatalf("status %d, want 404: %v", status, out)
	}
	if res := h.mcpCall("vacuum", map[string]any{"namespace": "nosuchns"}); !res.isError() {
		t.Fatalf("mcp vacuum of a missing namespace must fail: %v", res.result)
	}
	if list := h.mustHTTP("list_namespaces", map[string]any{}); strings.Contains(mustJSON(t, list), "nosuchns") {
		t.Fatal("vacuum must not create the namespace")
	}
}

func vacuumFixture(t *testing.T) (*harness, *sql.Conn) {
	t.Helper()
	sqliteOnly(t)
	h := newHarness(t)
	h.seedTable("vacheld", "notes", []map[string]any{{"name": "body", "type": "text"}})
	h.mustHTTP("insert", map[string]any{"namespace": "vacheld", "table": "notes", "records": []map[string]any{{"body": "x"}}})
	db, err := sql.Open("sqlite", "file:"+h.dir+"/vacheld.db?mode=rw&_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return h, conn
}

func assertVacuumTeaches(t *testing.T, h *harness, want string) {
	t.Helper()
	status, out := h.httpCall("vacuum", map[string]any{"namespace": "vacheld"})
	errEnv, _ := out["error"].(map[string]any)
	msg, _ := errEnv["message"].(string)
	if status == 200 || errEnv["code"] != "conflict" || !strings.Contains(msg, want) || strings.Contains(msg, h.dir) {
		t.Fatalf("vacuum must answer conflict naming %q without the file path, got %d %v", want, status, out)
	}
	res := h.mcpCall("vacuum", map[string]any{"namespace": "vacheld"})
	if !res.isError() || !strings.Contains(mustJSON(t, res.result), want) {
		t.Fatalf("mcp vacuum must carry the same teaching message, got %v", res.result)
	}
}

func TestVacuumReportsAReaderHoldingTheLog(t *testing.T) {
	h, conn := vacuumFixture(t)
	ctx := t.Context()
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM notes").Scan(&n); err != nil {
		t.Fatal(err)
	}
	assertVacuumTeaches(t, h, "retry vacuum once those reads finish")
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	h.mustHTTP("vacuum", map[string]any{"namespace": "vacheld"})
}

func TestVacuumTeachesWhenAnotherWriterHoldsTheNamespace(t *testing.T) {
	h, conn := vacuumFixture(t)
	ctx := t.Context()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	assertVacuumTeaches(t, h, "another process")
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}
