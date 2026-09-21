package conformance

import (
	"net/http"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/store"
)

func seedTwoTables(t *testing.T, h *harness) {
	t.Helper()
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	h.mustHTTP("create_table", map[string]any{
		"namespace": "acme", "table": "notes",
		"fields": []map[string]any{{"name": "body", "type": "text", "fulltext": true}, {"name": "rank", "type": "number"}},
	})
	h.mustHTTP("create_table", map[string]any{
		"namespace": "acme", "table": "payroll",
		"fields": []map[string]any{{"name": "salary", "type": "number"}},
	})
	h.mustHTTP("insert", map[string]any{
		"namespace": "acme", "table": "notes",
		"records": []map[string]any{{"body": "first note", "rank": 1}, {"body": "second note", "rank": 2}},
	})
	h.mustHTTP("insert", map[string]any{
		"namespace": "acme", "table": "payroll",
		"records": []map[string]any{{"salary": 100000}},
	})
}

func TestAuthOffKeepsTheV020FilterLanguage(t *testing.T) {
	filterLanguageEngineGap(t, "a filter subquery reaching a sibling table")
	h := newHarnessMode(t, authOff)
	seedTwoTables(t, h)

	status, out := h.httpCall("delete", map[string]any{
		"namespace": "acme", "table": "notes",
		"filter": "rank IN (SELECT salary FROM payroll)", "dry_run": true,
	})
	if status != http.StatusOK {
		t.Fatalf("auth off must keep the v0.2.0 filter language, subqueries included: %d %v", status, out)
	}
}

func TestAuthOnRefusesAFilterThatReadsBeyondTheRow(t *testing.T) {
	h := newHarnessMode(t, authGateway)
	seedTwoTables(t, h)
	grantTo(t, h, "principal", "alice", "acme", "notes", "read", "delete", "update")

	for _, tc := range []struct{ what, filter string }{
		{"a subquery", "rank IN (SELECT salary FROM payroll)"},
		{"an exists subquery", "EXISTS (SELECT 1 FROM payroll WHERE salary > 1)"},
		{"a cross-table reference", "payroll.salary > 1"},
		{"an unknown function", "random() > 0"},
		{"a nondeterministic clock read", "body > datetime('now')"},
	} {
		res, out := h.asIdentity(t, "alice", "", "delete",
			`{"namespace":"acme","table":"notes","filter":`+quote(tc.filter)+`,"dry_run":true}`)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s in a filter answered %d, want 400: %v", tc.what, res.StatusCode, out)
		}
		errObj, _ := out["error"].(map[string]any)
		if errObj["code"] != "invalid_request" {
			t.Fatalf("%s in a filter is not invalid_request: %v", tc.what, out)
		}
		if msg, _ := errObj["message"].(string); !strings.Contains(msg, "filter") {
			t.Fatalf("%s: the refusal does not say it is about the filter: %v", tc.what, out)
		}
	}
}

func TestATableGrantDoesNotReachAnotherTableThroughAFilter(t *testing.T) {
	h := newHarnessMode(t, authGateway)
	seedTwoTables(t, h)
	grantTo(t, h, "principal", "alice", "acme", "notes", "read", "delete")

	res, out := h.asIdentity(t, "alice", "", "delete",
		`{"namespace":"acme","table":"notes","filter":"(SELECT salary FROM payroll LIMIT 1) > 1","dry_run":true}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a grant on one table reached another through a filter subquery: %d %v", res.StatusCode, out)
	}
	res, out = h.asIdentity(t, "alice", "", "read_rows",
		`{"namespace":"acme","table":"payroll","ids":[1]}`)
	if res.StatusCode == http.StatusOK {
		t.Fatalf("the test is not proving anything: alice can read payroll directly: %v", out)
	}
}

func TestAuthOnKeepsTheAllowlistWorking(t *testing.T) {
	h := newHarnessMode(t, authGateway)
	seedTwoTables(t, h)
	grantTo(t, h, "principal", "alice", "acme", "notes", "read", "delete")

	res, out := h.asIdentity(t, "alice", "", "delete",
		`{"namespace":"acme","table":"notes","filter":"lower(body) LIKE ? AND rank BETWEEN 1 AND 9","args":["first%"],"dry_run":true}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("an allowlisted filter was refused under auth on: %d %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if matched, _ := data["matched"].(float64); matched != 1 {
		t.Fatalf("the allowlisted filter matched %v rows, want 1: %v", data["matched"], out)
	}
}

func TestABoundClockWordIsRefused(t *testing.T) {
	filterLanguageEngineGap(t, "the date and time functions")
	h := newHarnessMode(t, authGateway)
	seedTwoTables(t, h)
	grantTo(t, h, "principal", "alice", "acme", "notes", "read", "delete")

	res, out := h.asIdentity(t, "alice", "", "delete",
		`{"namespace":"acme","table":"notes","filter":"body > datetime(?)","args":["now"],"dry_run":true}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("binding 'now' as an argument smuggled the server clock into a filter: %d %v", res.StatusCode, out)
	}
	res, out = h.asIdentity(t, "alice", "", "delete",
		`{"namespace":"acme","table":"notes","filter":"body > datetime(?)","args":["2026-01-01T00:00:00Z"],"dry_run":true}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("an explicit moment bound as an argument was refused: %d %v", res.StatusCode, out)
	}
}

func TestTheFilterLanguageAppliesToEveryFilteredOp(t *testing.T) {
	h := newHarnessMode(t, authGateway)
	seedTwoTables(t, h)
	grantTo(t, h, "principal", "alice", "acme", "notes", "read", "update", "delete", "create")

	for _, tc := range []struct{ op, body string }{
		{"delete", `{"namespace":"acme","table":"notes","filter":"random() > 0","dry_run":true}`},
		{"update", `{"namespace":"acme","table":"notes","filter":"random() > 0","set":{"rank":9}}`},
		{"upsert", `{"namespace":"acme","table":"notes","filter":"random() > 0","set":{"body":"x","rank":9}}`},
		{"search_fulltext", `{"namespace":"acme","table":"notes","query":"note","filter":"random() > 0"}`},
	} {
		res, out := h.asIdentity(t, "alice", "", tc.op, tc.body)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s accepted a filter outside the allowlist: %d %v", tc.op, res.StatusCode, out)
		}
	}
}

func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func TestTheAdminKeyGetsTheSameFilterLanguage(t *testing.T) {
	h := newHarnessMode(t, authGateway)
	seedTwoTables(t, h)

	status, out := h.httpCall("delete", map[string]any{
		"namespace": "acme", "table": "notes",
		"filter": "rank IN (SELECT salary FROM payroll)", "dry_run": true,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("the admin key was exempted from the filter language, which the spec states without a carve-out: %d %v", status, out)
	}
	status, out = h.httpCall("delete", map[string]any{
		"namespace": "acme", "table": "notes",
		"filter": "rank > ?", "args": []any{1}, "dry_run": true,
	})
	if status != http.StatusOK {
		t.Fatalf("an allowlisted filter was refused for the admin key: %d %v", status, out)
	}
}

func TestADeeplyNestedFilterIsRefusedNotFatal(t *testing.T) {
	h := newHarnessMode(t, authGateway)
	seedTwoTables(t, h)
	grantTo(t, h, "principal", "alice", "acme", "notes", "read", "delete")

	deep := strings.Repeat("(", 5000) + "1=1" + strings.Repeat(")", 5000)
	res, out := h.asIdentity(t, "alice", "", "delete",
		`{"namespace":"acme","table":"notes","filter":`+quote(deep)+`,"dry_run":true}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a filter nested thousands deep answered %d; if the parser had recursed through it the process would be gone: %v", res.StatusCode, out)
	}

	status, out := h.httpCall("describe_table", map[string]any{"namespace": "acme", "table": "notes"})
	if status != http.StatusOK {
		t.Fatalf("the server did not survive the nested filter: %d %v", status, out)
	}
}

func filterLanguageEngineGap(t *testing.T, missing string) {
	t.Helper()
	if testEngine(t) != store.EnginePostgres {
		return
	}
	t.Skipf("engine %q does not implement %s yet; the spec requires one shared evaluator rather than a per-engine subset, so this is a gap to close and not a recorded divergence", store.EnginePostgres, missing)
}
