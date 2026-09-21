package conformance

import (
	"encoding/json"
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

func TestScopedMutationsReachOnlyOwnRows(t *testing.T) {
	h := seedRowAccess(t)
	grantTo(t, h, "principal", "alice", "acme", "notes", "create", "update", "delete")
	grantTo(t, h, "principal", "bob", "acme", "notes", "create")
	h.asIdentity(t, "alice", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"alice note"}]}`)
	h.asIdentity(t, "bob", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"bob note"}]}`)

	res, out := h.asIdentity(t, "alice", "", "update",
		`{"namespace":"acme","table":"notes","filter":"1=1","set":{"body":"rewritten"}}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("a scoped update was refused: %d %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if updated, _ := data["updated"].(float64); updated != 1 {
		t.Fatalf("a scoped update touched %v rows, want alice's 1: %v", data["updated"], out)
	}

	res, out = h.asIdentity(t, "alice", "", "delete",
		`{"namespace":"acme","table":"notes","filter":"1=1","confirm":true}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("a scoped delete was refused: %d %v", res.StatusCode, out)
	}
	data, _ = out["data"].(map[string]any)
	if deleted, _ := data["deleted"].(float64); deleted != 1 {
		t.Fatalf("a scoped delete removed %v rows, want alice's 1: %v", data["deleted"], out)
	}

	rows := h.mustHTTP("read_rows", map[string]any{"namespace": "acme", "table": "notes", "ids": []int64{1, 2}})
	remaining, _ := rows["rows"].([]any)
	if len(remaining) != 1 {
		t.Fatalf("the table holds %d rows after alice deleted her own, want bob's 1: %v", len(remaining), rows)
	}
	row, _ := remaining[0].(map[string]any)
	if row["body"] != "bob note" {
		t.Fatalf("a scoped mutation reached a foreign row: %v", row)
	}
}

func TestAScopedFilterCannotRaiseOnAForeignRow(t *testing.T) {
	h := seedRowAccess(t)
	grantTo(t, h, "principal", "alice", "acme", "notes", "create", "delete")
	grantTo(t, h, "principal", "bob", "acme", "notes", "create")
	h.asIdentity(t, "alice", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"alice note"}]}`)
	h.asIdentity(t, "bob", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"secret"}]}`)

	body := `{"namespace":"acme","table":"notes","filter":"iif(body = ?, abs(-9223372036854775808), 1) = 1","args":["secret"],"dry_run":true}`
	res, out := h.asIdentity(t, "alice", "", "delete", body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("a filter that overflows on bob's row reached it, and its error answers whether bob's row holds %q: %d %v", "secret", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if matched, _ := data["matched"].(float64); matched != 1 {
		t.Fatalf("the scoped dry run matched %v rows, want alice's 1: %v", data["matched"], out)
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

func TestChangeFeedsStillRequireTableWideRead(t *testing.T) {
	h := seedRowAccess(t)
	grantTo(t, h, "principal", "alice", "acme", "notes", "create")
	h.asIdentity(t, "alice", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"x"}]}`)

	for _, op := range []string{"changes_since", "wait_for"} {
		res, out := h.asIdentity(t, "alice", "", op, `{"namespace":"acme","table":"notes","cursor":"begin"}`)
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("%s as a create-only holder: status %d, want 403 until the feed honors a row scope: %v", op, res.StatusCode, out)
		}
	}
}

func TestMigrationKeepsTheScopeOnARowAccessTable(t *testing.T) {
	h := seedRowAccess(t)
	grantTo(t, h, "principal", "alice", "acme", "notes", "create")
	grantTo(t, h, "principal", "bob", "acme", "notes", "create")
	h.asIdentity(t, "alice", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"alice note"}]}`)
	h.asIdentity(t, "bob", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"bob note"}]}`)

	h.mustHTTP("migrate", map[string]any{
		"namespace": "acme", "table": "notes",
		"changes": []map[string]any{{"op": "add_field", "field": map[string]any{"name": "tag", "type": "string"}}},
	})

	res, out := h.asIdentity(t, "alice", "", "read_rows", `{"namespace":"acme","table":"notes","ids":[1,2]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("read after migration: status %d %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	rows, _ := data["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("after a migration alice saw %d rows, want only her own: %v", len(rows), rows)
	}
}

func TestSetRowAccessIsUnknownUnderAuthOff(t *testing.T) {
	h := newHarnessMode(t, authOff)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	h.seedTable("acme", "notes", []map[string]any{{"name": "body", "type": "text"}})

	status, out := h.httpCall("migrate", map[string]any{
		"namespace": "acme", "table": "notes",
		"changes": []map[string]any{{"op": "set_row_access", "value": true}},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("set_row_access under auth off: status %d, want 400: %v", status, out)
	}
	if doc := h.mustHTTPGet(t, "/v1/openapi.json"); strings.Contains(doc, "set_row_access") {
		t.Fatal("openapi.json advertises set_row_access under auth off")
	}
}

func TestEnablingRowAccessIsRefusedOnAPopulatedTable(t *testing.T) {
	h := newHarnessMode(t, authGateway)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	h.mustHTTP("create_table", map[string]any{
		"namespace": "acme", "table": "notes",
		"fields": []map[string]any{{"name": "body", "type": "text"}},
	})

	h.mustHTTP("migrate", map[string]any{
		"namespace": "acme", "table": "notes",
		"changes": []map[string]any{{"op": "set_row_access", "value": true}},
	})
	h.mustHTTP("migrate", map[string]any{
		"namespace": "acme", "table": "notes",
		"changes": []map[string]any{{"op": "set_row_access", "value": false}},
	})

	h.mustHTTP("insert", map[string]any{
		"namespace": "acme", "table": "notes",
		"records": []map[string]any{{"body": "x"}},
	})
	status, out := h.httpCall("migrate", map[string]any{
		"namespace": "acme", "table": "notes",
		"changes": []map[string]any{{"op": "set_row_access", "value": true}},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("enabling row_access on a populated table: status %d, want 400: %v", status, out)
	}
	errEnv, _ := out["error"].(map[string]any)
	if msg, _ := errEnv["message"].(string); !strings.Contains(msg, "replay") {
		t.Fatalf("the refusal does not teach the supported path: %v", out)
	}
}

func TestDisablingRowAccessNeedsAdminAndRead(t *testing.T) {
	h := seedRowAccess(t)
	grantTo(t, h, "principal", "sam", "acme", "notes", "schema", "read")

	res, out := h.asIdentity(t, "sam", "", "migrate",
		`{"namespace":"acme","table":"notes","changes":[{"op":"set_row_access","value":false}]}`)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("schema+read disabling row_access: status %d, want 403: %v", res.StatusCode, out)
	}
	errEnv, _ := out["error"].(map[string]any)
	if msg, _ := errEnv["message"].(string); !strings.Contains(msg, "admin") {
		t.Fatalf("the refusal does not name the missing verb: %v", out)
	}

	grantTo(t, h, "principal", "sam", "acme", "notes", "admin")
	res, out = h.asIdentity(t, "sam", "", "migrate",
		`{"namespace":"acme","table":"notes","changes":[{"op":"set_row_access","value":false}]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("schema+read+admin disabling row_access: status %d %v", res.StatusCode, out)
	}
}

func TestEnablingRowAccessNeedsReadBeforeTheRowCountIsConsulted(t *testing.T) {
	h := newHarnessMode(t, authGateway)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	h.mustHTTP("create_table", map[string]any{
		"namespace": "acme", "table": "notes",
		"fields": []map[string]any{{"name": "body", "type": "text"}},
	})
	h.mustHTTP("insert", map[string]any{
		"namespace": "acme", "table": "notes",
		"records": []map[string]any{{"body": "secret"}},
	})
	grantTo(t, h, "principal", "dave", "acme", "notes", "schema")

	res, out := h.asIdentity(t, "dave", "", "migrate",
		`{"namespace":"acme","table":"notes","changes":[{"op":"set_row_access","value":true}]}`)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("a schema-only caller: status %d, want 403 before any row-dependent check: %v", res.StatusCode, out)
	}
	body := mustJSON(t, out)
	if strings.Contains(body, "1 rows") || strings.Contains(body, "already holds") {
		t.Fatalf("the refusal leaked the row count to a schema-only caller: %s", body)
	}
}

func TestDisablingRowAccessStopsTheFiltering(t *testing.T) {
	h := seedRowAccess(t)
	grantTo(t, h, "principal", "alice", "acme", "notes", "create")
	grantTo(t, h, "principal", "bob", "acme", "notes", "create")
	grantTo(t, h, "principal", "carol", "acme", "notes", "read")
	h.asIdentity(t, "alice", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"alice note"}]}`)
	h.asIdentity(t, "bob", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"bob note"}]}`)

	res, out := h.asIdentity(t, "alice", "", "read_rows", `{"namespace":"acme","table":"notes","ids":[1,2]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("alice read while row_access is on: status %d %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if rows, _ := data["rows"].([]any); len(rows) != 1 {
		t.Fatalf("alice saw %d rows while scoped, want 1", len(rows))
	}

	h.mustHTTP("migrate", map[string]any{
		"namespace": "acme", "table": "notes",
		"changes": []map[string]any{{"op": "set_row_access", "value": false}},
	})

	res, out = h.asIdentity(t, "alice", "", "read_rows", `{"namespace":"acme","table":"notes","ids":[1,2]}`)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("with row_access off a create-only caller has no read path: status %d, want 403: %v", res.StatusCode, out)
	}

	res, out = h.asIdentity(t, "carol", "", "read_rows", `{"namespace":"acme","table":"notes","ids":[1,2]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("carol read after disabling: status %d %v", res.StatusCode, out)
	}
	data, _ = out["data"].(map[string]any)
	rows, _ := data["rows"].([]any)
	if len(rows) != 2 {
		t.Fatalf("a read holder saw %d rows after disabling, want the whole table", len(rows))
	}
	kept := 0
	for _, r := range rows {
		if row, _ := r.(map[string]any); row["owner"] != nil {
			kept++
		}
	}
	if kept != 2 {
		t.Fatalf("disabling row_access dropped stored owner values: %v", rows)
	}
}

func TestOwnerStaysReservedWhileTheColumnExists(t *testing.T) {
	h := seedRowAccess(t)

	for _, change := range []map[string]any{
		{"op": "add_field", "field": map[string]any{"name": "owner", "type": "string"}},
		{"op": "rename_field", "from": "body", "to": "owner"},
	} {
		status, out := h.httpCall("migrate", map[string]any{
			"namespace": "acme", "table": "notes", "changes": []map[string]any{change},
		})
		if status != http.StatusBadRequest {
			t.Fatalf("%v: status %d, want 400: %v", change["op"], status, out)
		}
	}

	h.mustHTTP("migrate", map[string]any{
		"namespace": "acme", "table": "notes",
		"changes": []map[string]any{{"op": "set_row_access", "value": false}},
	})
	status, out := h.httpCall("migrate", map[string]any{
		"namespace": "acme", "table": "notes",
		"changes": []map[string]any{{"op": "add_field", "field": map[string]any{"name": "owner", "type": "string"}}},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("owner must stay reserved while the column exists: status %d %v", status, out)
	}
}

func TestAdvertisedMigrateSchemaAcceptsSetRowAccess(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)
	doc := h.mustHTTPGet(t, "/v1/openapi.json")

	if !strings.Contains(doc, "set_row_access") {
		t.Fatal("openapi.json omits set_row_access under auth on")
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
		t.Fatalf("openapi.json is not valid JSON: %v", err)
	}

	paths, _ := parsed["paths"].(map[string]any)
	migrate, _ := paths["/v1/migrate"].(map[string]any)
	post, _ := migrate["post"].(map[string]any)
	reqBody, _ := post["requestBody"].(map[string]any)
	content, _ := reqBody["content"].(map[string]any)
	appJSON, _ := content["application/json"].(map[string]any)
	sc, _ := appJSON["schema"].(map[string]any)
	props, _ := sc["properties"].(map[string]any)
	changes, _ := props["changes"].(map[string]any)
	items, _ := changes["items"].(map[string]any)

	raw := mustJSON(t, items)
	if strings.Contains(raw, `["set_fulltext","set_vectorize"]`) {
		t.Fatalf("the advertised schema still forbids value outside set_fulltext/set_vectorize, so every set_row_access request it describes is invalid: %s", raw)
	}
	if !strings.Contains(raw, "set_row_access") {
		t.Fatalf("the advertised change schema never mentions set_row_access: %s", raw)
	}
}

func TestScopedInsertRefusesAnIdempotencyKey(t *testing.T) {
	h := seedRowAccess(t)
	grantTo(t, h, "principal", "alice", "acme", "notes", "create")
	grantTo(t, h, "principal", "bob", "acme", "notes", "create")

	res, out := h.asIdentity(t, "bob", "", "insert",
		`{"namespace":"acme","table":"notes","records":[{"body":"bob's"}],"idempotency_key":"shared-key"}`)
	if res.StatusCode == http.StatusOK {
		t.Fatalf("a scoped insert accepted an idempotency key, which is recorded per table and would report another owner's rows on replay: %v", out)
	}

	res, out = h.asIdentity(t, "alice", "", "insert",
		`{"namespace":"acme","table":"notes","records":[{"body":"bob's"}],"idempotency_key":"shared-key"}`)
	if res.StatusCode == http.StatusOK {
		t.Fatalf("alice replayed bob's key: %v", out)
	}

	res, out = h.asIdentity(t, "alice", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"x"}]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("a scoped insert without a key must still work: status %d %v", res.StatusCode, out)
	}
}

func TestDisablingRowAccessClosesTheReadPathButNotTheRows(t *testing.T) {
	h := seedRowAccess(t)
	grantTo(t, h, "principal", "alice", "acme", "notes", "create")
	h.asIdentity(t, "alice", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"alice note"}]}`)
	h.mustHTTP("insert", map[string]any{"namespace": "acme", "table": "notes", "records": []map[string]any{{"body": "someone else"}}})

	h.mustHTTP("migrate", map[string]any{
		"namespace": "acme", "table": "notes",
		"changes": []map[string]any{{"op": "set_row_access", "value": false}},
	})

	for _, tc := range []struct{ op, body string }{
		{"read_rows", `{"namespace":"acme","table":"notes","ids":[1,2]}`},
		{"search_fulltext", `{"namespace":"acme","table":"notes","query":"note"}`},
	} {
		res, out := h.asIdentity(t, "alice", "", tc.op, tc.body)
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("%s answered %d: disabling row_access must not re-open the read path to a holder of data verbs alone: %v", tc.op, res.StatusCode, out)
		}
	}

	res, out := h.asIdentity(t, "alice", "", "describe_table", `{"namespace":"acme","table":"notes"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("describe_table needs any verb: status %d %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if got := int64val(t, "row_count", data["row_count"]); got != 2 {
		t.Fatalf("row_count %d, want the whole table: a table without row_access has no row-level protection left, and the count follows the visible set", got)
	}
}

func TestMutationsReachEveryRowOnceRowAccessIsOff(t *testing.T) {
	h := seedRowAccess(t)
	grantTo(t, h, "principal", "alice", "acme", "notes", "create", "update", "delete")
	h.asIdentity(t, "alice", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"alice note"}]}`)
	h.mustHTTP("insert", map[string]any{"namespace": "acme", "table": "notes", "records": []map[string]any{{"body": "someone else"}}})

	h.mustHTTP("migrate", map[string]any{
		"namespace": "acme", "table": "notes",
		"changes": []map[string]any{{"op": "set_row_access", "value": false}},
	})

	res, out := h.asIdentity(t, "alice", "", "update",
		`{"namespace":"acme","table":"notes","filter":"1=1","set":{"body":"rewritten"}}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("update: %d %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	if updated, _ := data["updated"].(float64); updated != 2 {
		t.Fatalf("update reported %v rows, want the whole table: an empty scope here silently reports nothing changed", data["updated"])
	}

	res, out = h.asIdentity(t, "alice", "", "upsert",
		`{"namespace":"acme","table":"notes","filter":"body = ?","args":["rewritten"],"set":{"body":"rewritten"}}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("upsert: %d %v", res.StatusCode, out)
	}
	data, _ = out["data"].(map[string]any)
	if inserted, _ := data["inserted"].(float64); inserted != 0 {
		t.Fatalf("upsert inserted %v rows though its filter matched: an empty scope makes every upsert take the no-match branch and duplicate", data["inserted"])
	}

	res, out = h.asIdentity(t, "alice", "", "delete",
		`{"namespace":"acme","table":"notes","filter":"1=1","confirm":true}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("delete: %d %v", res.StatusCode, out)
	}
	data, _ = out["data"].(map[string]any)
	if deleted, _ := data["deleted"].(float64); deleted != 2 {
		t.Fatalf("delete removed %v rows, want the whole table", data["deleted"])
	}
}
