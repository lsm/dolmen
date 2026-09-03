package api

import (
	"encoding/json"
	"net/http"
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
	doc := New(nil, fakeEmb{}).OpenAPIDoc()
	assertOpenAPIDoc(t, doc)
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
	doc1 := New(nil, fakeEmb{}).OpenAPIDoc()
	doc2 := New(nil, fakeEmb{}).OpenAPIDoc()
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
