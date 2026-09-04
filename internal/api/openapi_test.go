package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

func TestOpenAPIEndpoint(t *testing.T) {
	srv := newTestServer(t)
	res, err := http.Get(srv.URL + "/v1/openapi.json")
	if err != nil {
		t.Fatalf("get openapi: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	if res.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("expected application/json, got %q", res.Header.Get("Content-Type"))
	}
	var doc map[string]any
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		t.Fatalf("decode openapi: %v", err)
	}
	assertOpenAPIDoc(t, doc)

	servers, ok := doc["servers"].([]any)
	if !ok || len(servers) == 0 {
		t.Fatalf("servers missing or empty")
	}
	server, ok := servers[0].(map[string]any)
	if !ok {
		t.Fatalf("server entry is not an object")
	}
	if server["url"] != srv.URL {
		t.Fatalf("server url without prefix: got %q, want %q", server["url"], srv.URL)
	}
}

func TestOpenAPIServerURLIncludesForwardedPrefix(t *testing.T) {
	srv := newTestServer(t)
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/openapi.json", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "public.example.com")
	req.Header.Set("X-Forwarded-Prefix", "/dolmen/")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get openapi: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	var doc map[string]any
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		t.Fatalf("decode openapi: %v", err)
	}
	servers, ok := doc["servers"].([]any)
	if !ok || len(servers) == 0 {
		t.Fatalf("servers missing or empty")
	}
	server, ok := servers[0].(map[string]any)
	if !ok {
		t.Fatalf("server entry is not an object")
	}
	if server["url"] != "https://public.example.com/dolmen" {
		t.Fatalf("server url: got %q, want %q", server["url"], "https://public.example.com/dolmen")
	}
	if _, ok := doc["paths"].(map[string]any)["/v1/query"]; !ok {
		t.Fatalf("missing path /v1/query")
	}
}

func TestOpenAPIErrorEnvelopeMatchesObjectErrors(t *testing.T) {
	doc := New(nil, fakeEmb{}).OpenAPIDoc("")
	components, ok := doc["components"].(map[string]any)
	if !ok {
		t.Fatalf("components missing")
	}
	schemas, ok := components["schemas"].(map[string]any)
	if !ok {
		t.Fatalf("components.schemas missing")
	}
	envelope, ok := schemas["ErrorEnvelope"].(map[string]any)
	if !ok {
		t.Fatalf("ErrorEnvelope missing")
	}
	props, ok := envelope["properties"].(map[string]any)
	if !ok {
		t.Fatalf("ErrorEnvelope has no properties")
	}
	// Failure responses carry a stable error object, not a bare string, so
	// generated clients deserialize code/message/request_id correctly.
	errObj, ok := props["error"].(map[string]any)
	if !ok || errObj["type"] != "object" {
		t.Fatalf("ErrorEnvelope.error must be an object schema, got %v", props["error"])
	}
	errProps, ok := errObj["properties"].(map[string]any)
	if !ok {
		t.Fatalf("ErrorEnvelope.error has no properties")
	}
	codeSchema, ok := errProps["code"].(map[string]any)
	if !ok {
		t.Fatalf("ErrorEnvelope.error.code missing")
	}
	enum, _ := codeSchema["enum"].([]string)
	got := map[string]bool{}
	for _, c := range enum {
		got[c] = true
	}
	for _, want := range errorCodeEnum {
		if !got[want] {
			t.Fatalf("ErrorEnvelope.error.code enum must contain %q, got %v", want, enum)
		}
	}
	if _, ok := errProps["message"]; !ok {
		t.Fatalf("ErrorEnvelope.error.message missing")
	}
	if _, ok := errProps["request_id"]; !ok {
		t.Fatalf("ErrorEnvelope.error.request_id missing")
	}
	req, _ := errObj["required"].([]string)
	reqSet := map[string]bool{}
	for _, r := range req {
		reqSet[r] = true
	}
	// request_id is always present (echoed or server-generated), so generated
	// clients may rely on it for log correlation.
	if !reqSet["code"] || !reqSet["message"] || !reqSet["request_id"] {
		t.Fatalf("ErrorEnvelope.error must require code, message, and request_id, got %v", req)
	}
}

func TestOpenAPIEndpointMethodNotAllowed(t *testing.T) {
	srv := newTestServer(t)
	res, err := http.Post(srv.URL+"/v1/openapi.json", "application/json", nil)
	if err != nil {
		t.Fatalf("post openapi: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", res.StatusCode)
	}
	if res.Header.Get("Allow") != http.MethodGet {
		t.Fatalf("expected Allow: GET, got %q", res.Header.Get("Allow"))
	}
}

func TestOpenAPICoversAllOps(t *testing.T) {
	doc := New(nil, fakeEmb{}).OpenAPIDoc("")
	assertOpenAPIDoc(t, doc)
}

// TestOpenAPIFieldDeclaresEnum pins the annotation's presence in the OpenAPI
// Field component — every client sees the vocabulary in the schema itself.
func TestOpenAPIFieldDeclaresEnum(t *testing.T) {
	doc := New(nil, fakeEmb{}).OpenAPIDoc("")
	field := doc["components"].(map[string]any)["schemas"].(map[string]any)["Field"].(map[string]any)
	enum, ok := field["properties"].(map[string]any)["enum"].(map[string]any)
	if !ok {
		t.Fatalf("Field component must declare an enum property, got %v", field["properties"])
	}
	if enum["type"] != "array" {
		t.Fatalf("Field.enum must be an array of strings, got %v", enum)
	}
	items, ok := enum["items"].(map[string]any)
	if !ok || items["type"] != "string" {
		t.Fatalf("Field.enum items must be strings, got %v", enum["items"])
	}
}

func assertOpenAPIDoc(t *testing.T, doc map[string]any) {
	t.Helper()
	if doc["openapi"] != "3.1.0" {
		t.Fatalf("expected openapi 3.1.0, got %v", doc["openapi"])
	}
	info, ok := doc["info"].(map[string]any)
	if !ok || info["title"] == "" || info["version"] == "" {
		t.Fatalf("info missing or incomplete: %v", doc["info"])
	}

	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatalf("paths missing or not an object: %v", doc["paths"])
	}
	if len(paths) != len(Ops) {
		t.Fatalf("expected %d paths, got %d", len(Ops), len(paths))
	}

	components, ok := doc["components"].(map[string]any)
	if !ok {
		t.Fatalf("components missing or not an object")
	}
	schemas, ok := components["schemas"].(map[string]any)
	if !ok {
		t.Fatalf("components.schemas missing or not an object")
	}
	for _, want := range []string{"ErrorEnvelope", "Field", "TableSchema", "Row"} {
		if _, ok := schemas[want]; !ok {
			t.Fatalf("missing component schema %q", want)
		}
	}

	for _, name := range OpNames() {
		pathItem, ok := paths["/v1/"+name]
		if !ok {
			t.Fatalf("missing path /v1/%s", name)
		}
		pathMap, ok := pathItem.(map[string]any)
		if !ok {
			t.Fatalf("path /v1/%s is not an object", name)
		}
		op, ok := pathMap["post"].(map[string]any)
		if !ok {
			t.Fatalf("missing post operation for /v1/%s", name)
		}
		if op["operationId"] != name {
			t.Fatalf("operationId for %s = %v, want %q", name, op["operationId"], name)
		}
		if op["summary"] == "" {
			t.Fatalf("summary missing for %s", name)
		}

		// Request body is required and uses the op's InputSchema.
		reqBody, ok := op["requestBody"].(map[string]any)
		if !ok {
			t.Fatalf("missing requestBody for %s", name)
		}
		if reqBody["required"] != true {
			t.Fatalf("requestBody for %s must be required", name)
		}
		reqContent, ok := reqBody["content"].(map[string]any)
		if !ok {
			t.Fatalf("missing requestBody content for %s", name)
		}
		reqJSON, ok := reqContent["application/json"].(map[string]any)
		if !ok {
			t.Fatalf("missing application/json request schema for %s", name)
		}
		reqSchema, ok := reqJSON["schema"].(map[string]any)
		if !ok || reqSchema["type"] != "object" {
			t.Fatalf("request schema for %s must be a JSON object", name)
		}

		// Every op must have an OutputSchema wired into the registry.
		def := Ops[name]
		if def.OutputSchema == nil {
			t.Fatalf("%s: OutputSchema not set in OpDef", name)
		}

		// 200 response wraps the op's OutputSchema in the ok/data envelope.
		responses, ok := op["responses"].(map[string]any)
		if !ok {
			t.Fatalf("missing responses for %s", name)
		}
		okResp, ok := responses["200"].(map[string]any)
		if !ok {
			t.Fatalf("missing 200 response for %s", name)
		}
		okContent, ok := okResp["content"].(map[string]any)
		if !ok {
			t.Fatalf("missing 200 content for %s", name)
		}
		okJSON, ok := okContent["application/json"].(map[string]any)
		if !ok {
			t.Fatalf("missing application/json 200 schema for %s", name)
		}
		okSchema, ok := okJSON["schema"].(map[string]any)
		if !ok {
			t.Fatalf("200 schema for %s is not an object", name)
		}
		opts, ok := okSchema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("200 schema for %s has no properties", name)
		}
		if opts["ok"] == nil || opts["data"] == nil {
			t.Fatalf("200 schema for %s must have ok and data properties", name)
		}
		dataSchema, ok := opts["data"].(map[string]any)
		if !ok {
			t.Fatalf("data schema for %s is not an object", name)
		}
		if dataSchema["type"] != "object" && dataSchema["$ref"] == nil {
			t.Fatalf("data schema for %s must be an object or a $ref, got %v", name, dataSchema)
		}

		for _, code := range []string{"400", "403", "404", "405", "413", "415", "500"} {
			if _, ok := responses[code]; !ok {
				t.Fatalf("missing %s response for %s", code, name)
			}
		}
	}
}

func TestOpenAPIWiredToOpsRegistry(t *testing.T) {
	for _, name := range OpNames() {
		def := Ops[name]
		if def.OutputSchema == nil {
			t.Fatalf("%s: OutputSchema must be wired to ops registry", name)
		}
		if def.OutputSchema["type"] != "object" {
			t.Fatalf("%s: OutputSchema must be a JSON object", name)
		}
	}
}

func TestOpenAPIOutputSchemasMarkRequiredGuaranteedFields(t *testing.T) {
	optional := map[string]bool{"replayed": true}
	for _, name := range OpNames() {
		def := Ops[name]
		if def.OutputSchema == nil {
			t.Fatalf("%s: OutputSchema must be wired to ops registry", name)
		}
		props, _ := def.OutputSchema["properties"].(map[string]any)
		req, _ := def.OutputSchema["required"].([]string)
		if len(req) == 0 {
			t.Fatalf("%s: OutputSchema must declare required fields", name)
		}
		required := map[string]bool{}
		for _, f := range req {
			required[f] = true
		}
		for prop := range props {
			if optional[prop] {
				if required[prop] {
					t.Fatalf("%s: conditional field %q must not be required", name, prop)
				}
				continue
			}
			if !required[prop] {
				t.Fatalf("%s: guaranteed field %q must be marked required", name, prop)
			}
		}
	}
}

// TestWriteOpsOutputSchemasShareShape keeps the three write ops documented in
// one shape: identical schemas for the ops that can update, and an insert
// schema that differs only by dropping updated (an insert cannot update) and
// carrying the optional replayed (only idempotent inserts can replay).
func TestWriteOpsOutputSchemasShareShape(t *testing.T) {
	if !reflect.DeepEqual(Ops["upsert"].OutputSchema, Ops["upsert_by_key"].OutputSchema) {
		t.Fatalf("upsert and upsert_by_key must share one output schema, got %v vs %v",
			Ops["upsert"].OutputSchema, Ops["upsert_by_key"].OutputSchema)
	}
	insProps, _ := Ops["insert"].OutputSchema["properties"].(map[string]any)
	upsProps, _ := Ops["upsert"].OutputSchema["properties"].(map[string]any)
	if _, ok := insProps["updated"]; ok {
		t.Fatalf("insert cannot update rows; its output schema must not declare updated: %v", insProps)
	}
	for _, k := range []string{"ids", "inserted", "replayed"} {
		if insProps[k] == nil {
			t.Fatalf("insert output schema must declare %s: %v", k, insProps)
		}
	}
	if _, ok := upsProps["replayed"]; ok {
		t.Fatalf("only idempotent inserts can replay; upsert output schema must not declare replayed: %v", upsProps)
	}
	for _, k := range []string{"ids", "inserted", "updated"} {
		if upsProps[k] == nil {
			t.Fatalf("upsert output schema must declare %s: %v", k, upsProps)
		}
	}
}

func TestOpenAPIIntegerFormatIsInt64(t *testing.T) {
	sc := integer(0)
	if sc["format"] != "int64" {
		t.Fatalf("integer() schema format = %v, want int64", sc["format"])
	}
	if sc["minimum"] != 0 {
		t.Fatalf("integer() schema minimum = %v, want 0", sc["minimum"])
	}
}

func TestOpenAPIPathNotUnknownOp(t *testing.T) {
	srv := newTestServer(t)
	// openapi.json must not be treated as an unknown op under /v1/.
	res, err := http.Get(srv.URL + "/v1/openapi.json")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for /v1/openapi.json, got %d", res.StatusCode)
	}
}

func TestOpenAPIIsStableJSON(t *testing.T) {
	doc1 := New(nil, fakeEmb{}).OpenAPIDoc("")
	doc2 := New(nil, fakeEmb{}).OpenAPIDoc("")
	raw1, err := json.Marshal(doc1)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw2, err := json.Marshal(doc2)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw1) != string(raw2) {
		t.Fatalf("OpenAPIDoc must be deterministic")
	}
}
