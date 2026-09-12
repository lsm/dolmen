package conformance

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestEnumWriteRejectionParity(t *testing.T) {
	h := newHarness(t)
	h.seedTable("voc", "incidents", []map[string]any{
		{"name": "title", "type": "string"},
		{"name": "severity", "type": "string", "enum": []string{"SEV0", "SEV1", "SEV2", "SEV3"}},
	})

	httpTable := h.mustHTTP("describe_table", map[string]any{"namespace": "voc", "table": "incidents"})["table"].(map[string]any)
	mcpTable := h.mustMCP("describe_table", map[string]any{"namespace": "voc", "table": "incidents"})["table"].(map[string]any)
	sev := func(table map[string]any) map[string]any {
		for _, f := range table["fields"].([]any) {
			m := f.(map[string]any)
			if m["name"] == "severity" {
				return m
			}
		}
		t.Fatalf("severity field missing: %v", table)
		return nil
	}
	assertJSONEqual(t, "severity over http", sev(httpTable), sev(mcpTable))
	got, _ := sev(httpTable)["enum"].([]any)
	if len(got) != 4 || got[0] != "SEV0" || got[3] != "SEV3" {
		t.Fatalf("describe_table must carry the declared enum verbatim, got %v", got)
	}

	bad := map[string]any{"namespace": "voc", "table": "incidents", "records": []map[string]any{{"title": "typo", "severity": "opn"}}}
	status, body := h.httpCall("insert", bad)
	if status != http.StatusBadRequest {
		t.Fatalf("insert non-member over http: status %d, want 400: %v", status, body)
	}
	httpMsg := envelopeOf(t, body)["message"].(string)
	wantMessage(t, "http rejection", httpMsg,
		`field "severity": value "opn" is not one of the allowed enum values \(SEV0, SEV1, SEV2, SEV3\)`)

	res := h.mcpCall("insert", map[string]any{
		"namespace": "voc", "table": "incidents",
		"records": []map[string]any{{"title": "typo", "severity": "opn"}},
	})
	if !res.isError() {
		t.Fatalf("insert non-member over MCP must fail: %+v", res)
	}
	mcpMsg := res.toolError()["message"].(string)
	if mcpMsg != httpMsg {
		t.Fatalf("rejection message must be identical over both transports:\nhttp: %q\nmcp:  %q", httpMsg, mcpMsg)
	}

	status, body = h.httpCall("update", map[string]any{
		"namespace": "voc", "table": "incidents", "filter": "1=1",
		"set": map[string]any{"severity": "urgent"},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("update non-member: status %d, want 400: %v", status, body)
	}
	wantMessage(t, "update rejection", envelopeOf(t, body)["message"].(string),
		`field "severity": value "urgent" is not one of the allowed enum values`)
	res = h.mcpCall("upsert_by_key", map[string]any{
		"namespace": "voc", "table": "incidents", "on": []string{"title"},
		"records": []map[string]any{{"title": "x", "severity": "sev1"}},
	})
	if !res.isError() {
		t.Fatalf("upsert_by_key non-member over MCP must fail: %+v", res)
	}
	wantMessage(t, "mcp upsert rejection", res.toolError()["message"].(string),
		`field "severity": value "sev1" is not one of the allowed enum values \(SEV0, SEV1, SEV2, SEV3\)`)

	out := h.mustHTTP("insert", map[string]any{
		"namespace": "voc", "table": "incidents",
		"records": []map[string]any{{"title": "real", "severity": "SEV1"}},
	})
	if ids := out["ids"].([]any); len(ids) != 1 {
		t.Fatalf("member insert must store: %v", out)
	}
	rows := h.mustHTTP("query", map[string]any{
		"namespace": "voc", "sql": "SELECT severity FROM incidents",
	})["rows"].([]any)
	if rows[0].(map[string]any)["severity"] != "SEV1" {
		t.Fatalf("member value must be stored as written: %v", rows)
	}
}

func TestEnumVisibleInSchemas(t *testing.T) {
	h := newHarness(t)

	list := h.rpc(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	tools := list.result["tools"].([]any)
	var createInput, migrateInput map[string]any
	for _, tl := range tools {
		tool := tl.(map[string]any)
		switch tool["name"] {
		case "create_table":
			createInput = tool["inputSchema"].(map[string]any)
		case "migrate":
			migrateInput = tool["inputSchema"].(map[string]any)
		}
	}
	if createInput == nil || migrateInput == nil {
		t.Fatalf("tools/list must include create_table and migrate: %v", tools)
	}
	items := createInput["properties"].(map[string]any)["fields"].(map[string]any)["items"].(map[string]any)
	enum, ok := items["properties"].(map[string]any)["enum"].(map[string]any)
	if !ok || enum["type"] != "array" || enum["minItems"] != float64(1) || enum["uniqueItems"] != true {
		t.Fatalf("tools/list create_table must declare the enum annotation, got %v", items["properties"])
	}
	changes := migrateInput["properties"].(map[string]any)["changes"].(map[string]any)["items"].(map[string]any)
	found := false
	for _, op := range changes["properties"].(map[string]any)["op"].(map[string]any)["enum"].([]any) {
		if op == "set_enum" {
			found = true
		}
	}
	if !found {
		t.Fatalf("tools/list migrate must list set_enum among its ops, got %v", changes)
	}

	res, err := http.Get(h.httpURL + "/openapi.json")
	if err != nil {
		t.Fatalf("get openapi.json: %v", err)
	}
	defer res.Body.Close()
	var doc map[string]any
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		t.Fatalf("decode openapi.json: %v", err)
	}
	field := doc["components"].(map[string]any)["schemas"].(map[string]any)["Field"].(map[string]any)
	fEnum, ok := field["properties"].(map[string]any)["enum"].(map[string]any)
	if !ok || fEnum["type"] != "array" || fEnum["items"].(map[string]any)["type"] != "string" {
		t.Fatalf("/v1/openapi.json Field must declare the enum property, got %v", field)
	}
}

func TestEnumSetEnumSemantics(t *testing.T) {
	h := newHarness(t)
	h.seedTable("voc", "incidents", []map[string]any{
		{"name": "title", "type": "string"},
		{"name": "severity", "type": "string"},
	})
	h.mustHTTP("insert", map[string]any{
		"namespace": "voc", "table": "incidents",
		"records": []map[string]any{
			{"title": "a", "severity": "SEV1"},
			{"title": "b", "severity": "SEV2"},
		},
	})

	status, body := h.httpCall("migrate", map[string]any{
		"namespace": "voc", "table": "incidents",
		"changes": []map[string]any{{"op": "set_enum", "name": "severity", "enum": []string{"SEV1"}}},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("constrain over non-member: status %d, want 400: %v", status, body)
	}
	wantMessage(t, "in-use value", envelopeOf(t, body)["message"].(string),
		`cannot apply this enum — "SEV2" is stored by 1 rows`)

	out := h.mustHTTP("migrate", map[string]any{
		"namespace": "voc", "table": "incidents",
		"changes": []map[string]any{{"op": "set_enum", "name": "severity", "enum": []string{"SEV1", "SEV2"}}},
	})
	if out["table"].(map[string]any)["version"] != float64(2) {
		t.Fatalf("set_enum must bump the version: %v", out)
	}

	status, body = h.httpCall("migrate", map[string]any{
		"namespace": "voc", "table": "incidents",
		"changes": []map[string]any{{"op": "set_enum", "name": "severity", "enum": []string{"SEV2"}}},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("remove in-use value: status %d, want 400: %v", status, body)
	}
	wantMessage(t, "in-use value", envelopeOf(t, body)["message"].(string),
		`"SEV1" is stored by 1 rows`)

	h.mustHTTP("migrate", map[string]any{
		"namespace": "voc", "table": "incidents",
		"changes": []map[string]any{{"op": "set_enum", "name": "severity", "enum": []string{"SEV1", "SEV2", "SEV3"}}},
	})
	h.mustHTTP("insert", map[string]any{
		"namespace": "voc", "table": "incidents",
		"records": []map[string]any{{"title": "c", "severity": "SEV3"}},
	})

	h.mustHTTP("migrate", map[string]any{
		"namespace": "voc", "table": "incidents",
		"changes": []map[string]any{{"op": "set_enum", "name": "severity", "enum": []string{}}},
	})
	h.mustHTTP("insert", map[string]any{
		"namespace": "voc", "table": "incidents",
		"records": []map[string]any{{"title": "free", "severity": "anything"}},
	})

	status, body = h.httpCall("create_table", map[string]any{
		"namespace": "voc", "table": "bad",
		"fields": []map[string]any{{"name": "severity", "type": "string", "enum": []string{"SEV0"}, "default": "SEV9"}},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("non-member default: status %d, want 400: %v", status, body)
	}
	wantMessage(t, "default membership", envelopeOf(t, body)["message"].(string),
		`field "severity": value "SEV9" is not one of the allowed enum values \(SEV0\)`)

	status, body = h.httpCall("migrate", map[string]any{
		"namespace": "voc", "table": "incidents",
		"changes": []map[string]any{{"op": "set_enum", "name": "severity"}},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("set_enum without enum: status %d, want 400: %v", status, body)
	}
	wantMessage(t, "missing enum key", envelopeOf(t, body)["message"].(string),
		`changes\[0\]: set_enum requires an explicit enum array`)
}
