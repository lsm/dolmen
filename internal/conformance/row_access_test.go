package conformance

import (
	"net/http"
	"strings"
	"testing"
)

func seedRowAccess(t *testing.T) *harness {
	t.Helper()
	h := newHarnessMode(t, authGateway)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	h.mustHTTP("create_table", map[string]any{
		"namespace":  "acme",
		"table":      "notes",
		"fields":     []map[string]any{{"name": "body", "type": "text", "fulltext": true}},
		"row_access": "own",
	})
	return h
}

func TestRowAccessIsUnknownUnderAuthOff(t *testing.T) {
	h := newHarnessMode(t, authOff)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	status, out := h.httpCall("create_table", map[string]any{
		"namespace":  "acme",
		"table":      "notes",
		"fields":     []map[string]any{{"name": "body", "type": "text"}},
		"row_access": "own",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("row_access under auth off: status %d, want 400: %v", status, out)
	}
	errEnv, _ := out["error"].(map[string]any)
	if msg, _ := errEnv["message"].(string); !strings.Contains(msg, "row_access") {
		t.Fatalf("the rejection does not name the unknown field: %v", out)
	}

	doc := h.mustHTTPGet(t, "/v1/openapi.json")
	if strings.Contains(doc, "row_access") {
		t.Fatal("openapi.json advertises row_access under auth off")
	}
}

func TestRowAccessIsAdvertisedUnderAuthOn(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)
	if doc := h.mustHTTPGet(t, "/v1/openapi.json"); !strings.Contains(doc, "row_access") {
		t.Fatal("openapi.json omits row_access under auth on")
	}
}

func TestScopedCallerSeesOnlyOwnRows(t *testing.T) {
	h := seedRowAccess(t)
	grantTo(t, h, "principal", "alice", "acme", "notes", "create")
	grantTo(t, h, "principal", "bob", "acme", "notes", "create")

	for _, who := range []string{"alice", "bob"} {
		res, out := h.asIdentity(t, who, "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"note from `+who+`"}]}`)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("insert as %s: status %d %v", who, res.StatusCode, out)
		}
	}

	res, out := h.asIdentity(t, "alice", "", "search_fulltext", `{"namespace":"acme","table":"notes","query":"note"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("alice search: status %d %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	results, _ := data["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("alice saw %d rows, want only her own: %v", len(results), results)
	}
	row, _ := results[0].(map[string]any)
	if body, _ := row["body"].(string); !strings.Contains(body, "alice") {
		t.Fatalf("alice saw a foreign row: %v", row)
	}
	if row["owner"] != "alice" {
		t.Fatalf("the owner column is not reported on a scoped read: %v", row)
	}

	res, out = h.asIdentity(t, "alice", "", "read_rows", `{"namespace":"acme","table":"notes","ids":[1,2]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("alice read_rows: status %d %v", res.StatusCode, out)
	}
	data, _ = out["data"].(map[string]any)
	rows, _ := data["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("alice read %d rows by id, want only her own: %v", len(rows), rows)
	}
}

func TestTableWideReaderSeesEveryRow(t *testing.T) {
	h := seedRowAccess(t)
	grantTo(t, h, "principal", "alice", "acme", "notes", "create")
	grantTo(t, h, "principal", "carol", "acme", "notes", "read")
	h.asIdentity(t, "alice", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"one"},{"body":"two"}]}`)

	res, out := h.asIdentity(t, "carol", "", "search_fulltext", `{"namespace":"acme","table":"notes","query":"one OR two"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("carol search: status %d %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if results, _ := data["results"].([]any); len(results) != 2 {
		t.Fatalf("a read holder saw %d rows, want the whole table", len(results))
	}
}

func TestSchemaOnlyHolderCountsZeroRows(t *testing.T) {
	h := seedRowAccess(t)
	grantTo(t, h, "principal", "alice", "acme", "notes", "create")
	grantTo(t, h, "principal", "dave", "acme", "notes", "schema")
	h.asIdentity(t, "alice", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"one"}]}`)

	res, out := h.asIdentity(t, "dave", "", "describe_table", `{"namespace":"acme","table":"notes"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("dave describe_table: status %d %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if got := int64val(t, "row_count", data["row_count"]); got != 0 {
		t.Fatalf("a schema-only holder saw row_count %d, want 0", got)
	}

	res, out = h.asIdentity(t, "alice", "", "describe_table", `{"namespace":"acme","table":"notes"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("alice describe_table: status %d %v", res.StatusCode, out)
	}
	data, _ = out["data"].(map[string]any)
	if got := int64val(t, "row_count", data["row_count"]); got != 1 {
		t.Fatalf("alice saw row_count %d, want her own 1", got)
	}
}

func TestOwnerIsNeverCallerSupplied(t *testing.T) {
	h := seedRowAccess(t)
	grantTo(t, h, "principal", "alice", "acme", "notes", "create")

	res, out := h.asIdentity(t, "alice", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"x","owner":"bob"}]}`)
	if res.StatusCode == http.StatusOK {
		t.Fatalf("a caller-supplied owner was accepted: %v", out)
	}
}

func TestScopedMutationsAreRefusedForNow(t *testing.T) {
	h := seedRowAccess(t)
	grantTo(t, h, "principal", "alice", "acme", "notes", "create", "update", "delete")
	h.asIdentity(t, "alice", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"x"}]}`)

	for _, tc := range []struct{ op, body string }{
		{"update", `{"namespace":"acme","table":"notes","filter":"1=1","set":{"body":"y"}}`},
		{"delete", `{"namespace":"acme","table":"notes","filter":"1=1","confirm":true}`},
	} {
		res, out := h.asIdentity(t, "alice", "", tc.op, tc.body)
		if res.StatusCode == http.StatusOK {
			t.Fatalf("%s executed under a row scope: %v", tc.op, out)
		}
		errEnv, _ := out["error"].(map[string]any)
		if msg, _ := errEnv["message"].(string); !strings.Contains(msg, "read verb") {
			t.Fatalf("%s refusal does not teach the way out: %v", tc.op, out)
		}
	}
}

func TestDefaultTablesCarryNoOwnerUnderAuthOn(t *testing.T) {
	h := newHarnessMode(t, authGateway)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	h.mustHTTP("create_table", map[string]any{
		"namespace": "acme", "table": "plain",
		"fields": []map[string]any{{"name": "body", "type": "text"}},
	})
	grantTo(t, h, "principal", "alice", "acme", "plain", "create", "read")
	h.asIdentity(t, "alice", "", "insert", `{"namespace":"acme","table":"plain","records":[{"body":"x"}]}`)

	res, out := h.asIdentity(t, "alice", "", "read_rows", `{"namespace":"acme","table":"plain","ids":[1]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("read_rows: status %d %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	rows, _ := data["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows %v", rows)
	}
	if row, _ := rows[0].(map[string]any); row["owner"] != nil {
		t.Fatalf("a default table reported an owner column: %v", row)
	}
}
