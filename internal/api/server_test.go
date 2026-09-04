package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
	"github.com/lsm/dolmen/internal/version"
)

type fakeEmb struct{}

func (fakeEmb) Name() string { return "fake" }

func (fakeEmb) ModelName() string { return "fake-model" }

func (fakeEmb) Identity() string { return "fake-space" }

func (fakeEmb) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, 8)
		for _, r := range []byte(t) {
			v[r%8] += 1
		}
		out[i] = v
	}
	return out, nil
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewServer(New(st, fakeEmb{}).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, base, op string, body any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	res, err := http.Post(base+"/v1/"+op, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("post %s: %v", op, err)
	}
	defer res.Body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode %s response: %v", op, err)
	}
	return res.StatusCode, decoded
}

func TestInferSchemaEndpoint(t *testing.T) {
	srv := newTestServer(t)
	long := fmt.Sprintf("A detailed finding body.%s", bytes.Repeat([]byte(" x"), 150))

	code, res := post(t, srv.URL, "infer_schema", map[string]any{
		"samples": []map[string]any{
			{"title": "bug", "score": 3.5, "ok": true, "at_time": "2026-09-01T10:00:00Z", "detail": long, "tags": []any{"a"}},
		},
	})
	if code != 200 {
		t.Fatalf("infer_schema failed: %d %v", code, res)
	}
	fields := res["data"].(map[string]any)["fields"].([]any)
	byName := map[string]map[string]any{}
	for _, f := range fields {
		m := f.(map[string]any)
		byName[m["name"].(string)] = m
	}
	if byName["score"]["type"] != "number" ||
		byName["ok"]["type"] != "boolean" ||
		byName["at_time"]["type"] != "timestamp" ||
		byName["tags"]["type"] != "json" {
		t.Fatalf("inferred types wrong: %v", byName)
	}
	if byName["detail"]["type"] != "text" || byName["detail"]["fulltext"] != true {
		t.Fatalf("long text should be text+fulltext: %v", byName["detail"])
	}
}

func TestOriginGuard(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	srv := httptest.NewServer(OriginGuard(New(st, fakeEmb{}).Handler(), []string{"https://app.example.com"}))
	t.Cleanup(srv.Close)

	do := func(origin, contentType string) int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/list_tables", bytes.NewReader([]byte(`{"namespace":"x"}`)))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		res.Body.Close()
		return res.StatusCode
	}

	if code := do("http://evil.example", "application/json"); code != http.StatusForbidden {
		t.Fatalf("cross-origin mutation must be rejected, got %d", code)
	}
	if code := do("http://localhost:5173", "application/json"); code != http.StatusOK {
		t.Fatalf("localhost origin must pass, got %d", code)
	}
	if code := do("", "application/json"); code != http.StatusOK {
		t.Fatalf("no-origin (curl/server) must pass, got %d", code)
	}
	if code := do("https://app.example.com", "application/json"); code != http.StatusOK {
		t.Fatalf("allowlisted origin must pass, got %d", code)
	}
	if code := do("", "text/plain"); code != http.StatusUnsupportedMediaType {
		t.Fatalf("non-JSON content type must be rejected, got %d", code)
	}

	res, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET healthz must pass guard, got %d", res.StatusCode)
	}
}

func TestCORSPreflight(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	srv := httptest.NewServer(OriginGuard(New(st, fakeEmb{}).Handler(), []string{"https://app.example.com"}))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/v1/insert", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent || res.Header.Get("Access-Control-Allow-Origin") != "https://app.example.com" {
		t.Fatalf("preflight for allowed origin failed: %d %q", res.StatusCode, res.Header.Get("Access-Control-Allow-Origin"))
	}
	allowed := res.Header.Get("Access-Control-Allow-Headers")
	if !strings.Contains(allowed, "X-Request-Id") {
		t.Fatalf("preflight must allow X-Request-Id header, got %q", allowed)
	}

	req, _ = http.NewRequest(http.MethodOptions, srv.URL+"/v1/insert", nil)
	req.Header.Set("Origin", "http://evil.example")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("preflight for disallowed origin must be 403, got %d", res.StatusCode)
	}

	// Actual cross-origin POST must expose the echoed X-Request-Id.
	raw, _ := json.Marshal(map[string]any{"namespace": "cors"})
	req, _ = http.NewRequest(http.MethodPost, srv.URL+"/v1/list_tables", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("X-Request-Id", "cors-req-1")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("cross-origin post: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("cross-origin post must 200, got %d", res.StatusCode)
	}
	if res.Header.Get("Access-Control-Allow-Origin") != "https://app.example.com" {
		t.Fatalf("allowed origin must be echoed, got %q", res.Header.Get("Access-Control-Allow-Origin"))
	}
	if res.Header.Get("Access-Control-Expose-Headers") != "X-Request-Id" {
		t.Fatalf("X-Request-Id must be exposed, got %q", res.Header.Get("Access-Control-Expose-Headers"))
	}
	if res.Header.Get("X-Request-Id") != "cors-req-1" {
		t.Fatalf("X-Request-Id must be echoed, got %q", res.Header.Get("X-Request-Id"))
	}
}

func TestTrailingContentRejected(t *testing.T) {
	srv := newTestServer(t)
	res, err := http.Post(srv.URL+"/v1/list_tables", "application/json",
		strings.NewReader(`{"namespace":"x"} {"namespace":"y"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for trailing content, got %d", res.StatusCode)
	}
}

func TestListTablesEmptyNamespaceReturnsArray(t *testing.T) {
	srv := newTestServer(t)
	code, body := post(t, srv.URL, "list_tables", map[string]any{"namespace": "fresh"})
	if code != http.StatusOK {
		t.Fatalf("list_tables on fresh namespace: %d %v", code, body)
	}
	data, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("expected data object, got %v", body)
	}
	tables, ok := data["tables"].([]any)
	if !ok {
		t.Fatalf(`"tables" must serialize as an array, got %T (%v)`, data["tables"], data["tables"])
	}
	if len(tables) != 0 {
		t.Fatalf("fresh namespace must list zero tables, got %v", tables)
	}
}

func TestCreateTableDimSchemaIsInteger(t *testing.T) {
	def, ok := Ops["create_table"]
	if !ok {
		t.Fatal("create_table op missing")
	}
	fields, ok := def.InputSchema["properties"].(map[string]any)["fields"].(map[string]any)
	if !ok {
		t.Fatal("fields property missing")
	}
	items := fields["items"].(map[string]any)
	dim := items["properties"].(map[string]any)["dim"].(map[string]any)
	if dim["type"] != "integer" {
		t.Fatalf(`"dim" must be declared integer (it decodes into an int field), got %v`, dim["type"])
	}
	if dim["minimum"] != 1 || dim["maximum"] != schema.MaxVectorDim {
		t.Fatalf(`"dim" must declare the accepted 1..%d range, got %v`, schema.MaxVectorDim, dim)
	}
}

func TestInferSchemaSampleBoundsDeclared(t *testing.T) {
	def, ok := Ops["infer_schema"]
	if !ok {
		t.Fatal("infer_schema op missing")
	}
	samples := def.InputSchema["properties"].(map[string]any)["samples"].(map[string]any)
	if samples["minItems"] != 1 || samples["maxItems"] != 50 {
		t.Fatalf(`"samples" must declare minItems 1 / maxItems 50 to match dispatch, got %v`, samples)
	}
}

func TestAllOpSchemasClosedToUnknownProperties(t *testing.T) {
	if len(Ops) != 19 {
		t.Fatalf("expected the nineteen ops, got %d", len(Ops))
	}
	for name, def := range Ops {
		if def.InputSchema["additionalProperties"] != false {
			t.Fatalf("%s: top-level schema must reject unknown properties", name)
		}
	}
}

func TestCreateTableFieldsMinItemsDeclared(t *testing.T) {
	def, ok := Ops["create_table"]
	if !ok {
		t.Fatal("create_table op missing")
	}
	fields := def.InputSchema["properties"].(map[string]any)["fields"].(map[string]any)
	if fields["minItems"] != 1 {
		t.Fatalf(`"fields" must declare minItems 1 to match schema.Validate, got %v`, fields)
	}
	if fields["maxItems"] != store.MaxFieldsPerTable {
		t.Fatalf(`"fields" must declare maxItems %d to match the store bound, got %v`, store.MaxFieldsPerTable, fields["maxItems"])
	}
	items := fields["items"].(map[string]any)
	if items["additionalProperties"] != false {
		t.Fatalf("field items must reject unknown properties, got %v", items["additionalProperties"])
	}
}

func TestUnknownRequestFieldsRejected(t *testing.T) {
	srv := newTestServer(t)
	code, body := post(t, srv.URL, "create_table", map[string]any{
		"namespace": "x",
		"table":     "typo",
		"fields":    []map[string]any{{"name": "title", "requred": true}},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("misspelled field option must 400, got %d %v", code, body)
	}
}

func TestCreateTableFieldTypeEnumDeclared(t *testing.T) {
	def, ok := Ops["create_table"]
	if !ok {
		t.Fatal("create_table op missing")
	}
	fields := def.InputSchema["properties"].(map[string]any)["fields"].(map[string]any)
	items := fields["items"].(map[string]any)
	typ := items["properties"].(map[string]any)["type"].(map[string]any)
	enum, ok := typ["enum"].([]schema.FieldType)
	if !ok || len(enum) != 7 {
		t.Fatalf(`field "type" must enumerate the seven supported types, got %v`, typ["enum"])
	}
	raw, err := json.Marshal(enum)
	if err != nil {
		t.Fatalf("marshal enum: %v", err)
	}
	for _, want := range []schema.FieldType{schema.String, schema.Text, schema.Number, schema.Boolean, schema.Timestamp, schema.JSON, schema.Vector} {
		if !strings.Contains(string(raw), string(want)) {
			t.Fatalf("enum must contain %q: %s", want, raw)
		}
	}
}

func TestCreateTableNameAndDimConstraintsDeclared(t *testing.T) {
	def, ok := Ops["create_table"]
	if !ok {
		t.Fatal("create_table op missing")
	}
	fields := def.InputSchema["properties"].(map[string]any)["fields"].(map[string]any)
	if fields["uniqueItems"] != true {
		t.Fatalf(`"fields" must declare uniqueItems to match duplicate-name rejection, got %v`, fields["uniqueItems"])
	}
	items := fields["items"].(map[string]any)
	name := items["properties"].(map[string]any)["name"].(map[string]any)
	if name["pattern"] != schema.IdentPattern() {
		t.Fatalf(`"name" must carry the ValidIdent pattern, got %v`, name["pattern"])
	}
	notEnum, ok := name["not"].(map[string]any)["enum"].([]string)
	if !ok || len(notEnum) != len(schema.ReservedFieldNames()) {
		t.Fatalf(`"name" must exclude the reserved field identifiers, got %v`, name["not"])
	}
	allOf, ok := items["allOf"].([]any)
	if !ok || len(allOf) != 5 {
		t.Fatalf("expected five conditional constraints (dim, fulltext, vectorize, default exclusions), got %v", items["allOf"])
	}
	dimRule := allOf[0].(map[string]any)
	then, ok := dimRule["then"].(map[string]any)["required"].([]string)
	if !ok || len(then) != 1 || then[0] != "dim" {
		t.Fatalf("vector fields must require dim via if/then, got %v", dimRule["then"])
	}
	elseNot, ok := dimRule["else"].(map[string]any)["not"].(map[string]any)["required"].([]string)
	if !ok || len(elseNot) != 1 || elseNot[0] != "dim" {
		t.Fatalf("non-vector fields must reject dim via if/else, got %v", dimRule["else"])
	}
}

func TestCreateTableFulltextAndVectorizeConstraintsDeclared(t *testing.T) {
	def, ok := Ops["create_table"]
	if !ok {
		t.Fatal("create_table op missing")
	}
	fields := def.InputSchema["properties"].(map[string]any)["fields"].(map[string]any)
	items := fields["items"].(map[string]any)
	allOf, ok := items["allOf"].([]any)
	if !ok || len(allOf) != 5 {
		t.Fatalf("expected five conditional constraints, got %v", items["allOf"])
	}
	fulltextThen := allOf[1].(map[string]any)["then"].(map[string]any)["properties"].(map[string]any)
	ftTypes, ok := fulltextThen["type"].(map[string]any)["enum"].([]schema.FieldType)
	if !ok || len(ftTypes) != 2 || ftTypes[0] != schema.String || ftTypes[1] != schema.Text {
		t.Fatalf("fulltext must be restricted to string/text, got %v", fulltextThen["type"])
	}
	if _, ok := fulltextThen["name"].(map[string]any)["not"].(map[string]any)["const"]; !ok {
		t.Fatalf("fulltext fields must exclude the FTS5-reserved name rank, got %v", fulltextThen["name"])
	}
	vectorizeThen := allOf[2].(map[string]any)["then"].(map[string]any)["properties"].(map[string]any)
	vzTypes, ok := vectorizeThen["type"].(map[string]any)["enum"].([]schema.FieldType)
	if !ok || len(vzTypes) != 2 || vzTypes[0] != schema.String || vzTypes[1] != schema.Text {
		t.Fatalf("vectorize must be restricted to string/text, got %v", vectorizeThen["type"])
	}
	notContains, ok := fields["not"].(map[string]any)
	if !ok {
		t.Fatalf("fields array must cap vectorize at one field, got %v", fields["not"])
	}
	if notContains["minContains"] != 2 {
		t.Fatalf("vectorize cap must use not/contains/minContains 2, got %v", notContains)
	}
	if _, ok := notContains["contains"].(map[string]any)["properties"].(map[string]any)["vectorize"]; !ok {
		t.Fatalf("vectorize cap must target the vectorize property, got %v", notContains["contains"])
	}
}

func TestCreateTableDefaultConstraintsDeclared(t *testing.T) {
	def, ok := Ops["create_table"]
	if !ok {
		t.Fatal("create_table op missing")
	}
	items := def.InputSchema["properties"].(map[string]any)["fields"].(map[string]any)["items"].(map[string]any)
	props := items["properties"].(map[string]any)
	if _, ok := props["default"].(map[string]any)["description"]; !ok {
		t.Fatalf(`create_table field items must declare "default", got %v`, props["default"])
	}
	allOf := items["allOf"].([]any)
	for i, exclude := range []string{"required", "vectorize"} {
		rule, ok := allOf[3+i].(map[string]any)
		if !ok {
			t.Fatalf("default exclusion %d must be an if/then rule, got %v", i, allOf[3+i])
		}
		ifCond := rule["if"].(map[string]any)
		if ifCond["properties"].(map[string]any)[exclude].(map[string]any)["const"] != true {
			t.Fatalf("default exclusion must key on %s=true, got %v", exclude, ifCond)
		}
		thenNot, ok := rule["then"].(map[string]any)["not"].(map[string]any)["required"].([]string)
		if !ok || len(thenNot) != 1 || thenNot[0] != "default" {
			t.Fatalf("%s=true must reject default via not/required, got %v", exclude, rule["then"])
		}
	}
	// add_field's backfill default lives on the change, so migrate's field
	// object must not declare one.
	migrateItems := Ops["migrate"].InputSchema["properties"].(map[string]any)["changes"].(map[string]any)["items"].(map[string]any)
	changeProps := migrateItems["properties"].(map[string]any)
	fieldProps := changeProps["field"].(map[string]any)["properties"].(map[string]any)
	if _, ok := fieldProps["default"]; ok {
		t.Fatalf("migrate's add_field field object must not declare default (it is the change's default), got %v", fieldProps["default"])
	}
	if _, ok := changeProps["default"].(map[string]any)["description"]; !ok {
		t.Fatalf("migrate's change object must declare its backfill default, got %v", changeProps["default"])
	}
}

func TestNamespaceAndTablePatternsDeclared(t *testing.T) {
	for _, op := range []string{"list_tables", "describe_table", "create_table"} {
		def, ok := Ops[op]
		if !ok {
			t.Fatalf("%s op missing", op)
		}
		props := def.InputSchema["properties"].(map[string]any)
		ns := props["namespace"].(map[string]any)
		if ns["pattern"] != `^[a-z0-9][a-z0-9_-]{0,63}$` {
			t.Fatalf("%s: namespace must carry the store ns pattern, got %v", op, ns["pattern"])
		}
		if op == "list_tables" {
			continue
		}
		table := props["table"].(map[string]any)
		notAnyOf, ok := table["not"].(map[string]any)["anyOf"].([]any)

		if !ok {
			t.Fatalf("%s: table must declare exclusions, got %v", op, table["not"])
		}
		if op == "create_table" {
			if table["pattern"] != schema.IdentPattern() {
				t.Fatalf("%s: table must carry the ValidIdent pattern, got %v", op, table["pattern"])
			}
			if len(notAnyOf) != 4 {
				t.Fatalf("%s: table must exclude __fts, sqlite_, pragma_, and reserved identifier names, got %v", op, table["not"])
			}
			if notAnyOf[2].(map[string]any)["pattern"] != "^pragma_" {
				t.Fatalf("%s: third exclusion must be ^pragma_, got %v", op, notAnyOf[2])
			}
			reservedEnum, ok := notAnyOf[3].(map[string]any)["enum"].([]string)
			wantReserved := schema.ReservedTableNames()
			if !ok || len(reservedEnum) != len(wantReserved) {
				t.Fatalf("%s: reserved table-name exclusions must cover %v, got %v", op, wantReserved, notAnyOf[2])
			}
			continue
		}
		// describe_table and other existing-table ops allow legacy keyword/reserved names.
		if table["pattern"] != `^[a-z][a-z0-9_]{0,63}$` {
			t.Fatalf("%s: table must carry the base identifier pattern for legacy names, got %v", op, table["pattern"])
		}
		if len(notAnyOf) != 2 {
			t.Fatalf("%s: table must exclude only __fts and sqlite_, got %v", op, table["not"])

		}
	}
}

func TestMethodNotAllowedSetsAllowHeader(t *testing.T) {
	srv := newTestServer(t)
	res, err := http.Get(srv.URL + "/v1/list_tables")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", res.StatusCode)
	}
	if res.Header.Get("Allow") != http.MethodPost {
		t.Fatalf(`405 must carry "Allow: POST", got %q`, res.Header.Get("Allow"))
	}
}

func TestRequestIdEchoedOnSuccess(t *testing.T) {
	srv := newTestServer(t)
	raw, _ := json.Marshal(map[string]any{"namespace": "x"})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/list_tables", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", "req-success-1")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}
	if res.Header.Get("X-Request-Id") != "req-success-1" {
		t.Fatalf("expected X-Request-Id echoed on success, got %q", res.Header.Get("X-Request-Id"))
	}
}

func TestUnknownOperationIs404ForAnyMethod(t *testing.T) {
	srv := newTestServer(t)
	res, err := http.Get(srv.URL + "/v1/no_such_op")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown operation must 404 regardless of method, got %d", res.StatusCode)
	}
	if res.Header.Get("Allow") != "" {
		t.Fatalf("404 must not advertise Allow, got %q", res.Header.Get("Allow"))
	}
}

func TestInferSchemaEmptyObjectsReturnArray(t *testing.T) {
	srv := newTestServer(t)
	code, body := post(t, srv.URL, "infer_schema", map[string]any{"samples": []map[string]any{{}}})
	if code != http.StatusOK {
		t.Fatalf("infer_schema with empty objects: %d %v", code, body)
	}
	data, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("expected data object, got %v", body)
	}
	fields, ok := data["fields"].([]any)
	if !ok {
		t.Fatalf(`"fields" must serialize as an array, got %T (%v)`, data["fields"], data["fields"])
	}
	if len(fields) != 0 {
		t.Fatalf("empty samples must infer zero fields, got %v", fields)
	}
}

func TestOversizedBodyReturns413(t *testing.T) {
	srv := newTestServer(t)
	big := bytes.Repeat([]byte("a"), 33<<20)
	res, err := http.Post(srv.URL+"/v1/list_tables", "application/json", bytes.NewReader(big))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body must return 413, got %d", res.StatusCode)
	}
}

func TestContentTypeParsedExactly(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewServer(OriginGuard(New(st, fakeEmb{}).Handler(), nil))
	t.Cleanup(srv.Close)
	for _, ct := range []string{"application/json", "application/json; charset=utf-8", "APPLICATION/JSON"} {
		res, err := http.Post(srv.URL+"/v1/list_tables", ct, strings.NewReader(`{"namespace":"x"}`))
		if err != nil {
			t.Fatalf("post %s: %v", ct, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("valid media type %q must be accepted, got %d", ct, res.StatusCode)
		}
	}
	for _, ct := range []string{"application/jsonp", "application/json-foo", "text/plain"} {
		res, err := http.Post(srv.URL+"/v1/list_tables", ct, strings.NewReader(`{"namespace":"x"}`))
		if err != nil {
			t.Fatalf("post %s: %v", ct, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnsupportedMediaType {
			t.Fatalf("invalid media type %q must be rejected with 415, got %d", ct, res.StatusCode)
		}
	}
}

func TestNullOptionValuesRejected(t *testing.T) {
	srv := newTestServer(t)
	res, err := http.Post(srv.URL+"/v1/create_table", "application/json",
		strings.NewReader(`{"namespace":"x","table":"nully","fields":[{"name":"title","type":null,"required":null}]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("null option values must 400 (they otherwise silently apply defaults), got %d", res.StatusCode)
	}
}

func TestInferSchemaSamplesAllowNulls(t *testing.T) {
	srv := newTestServer(t)
	code, body := post(t, srv.URL, "infer_schema", map[string]any{
		"samples": []map[string]any{{"name": "Alice"}, {"name": nil}},
	})
	if code != http.StatusOK {
		t.Fatalf("null observations inside samples are legitimate inference input: %d %v", code, body)
	}
	data, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("expected data object, got %v", body)
	}
	if fields, ok := data["fields"].([]any); !ok || len(fields) != 1 {
		t.Fatalf("null-only key must survive inference as a field, got %v", data["fields"])
	}
}

func TestInferSchemaNullSampleEntryRejected(t *testing.T) {
	srv := newTestServer(t)
	res, err := http.Post(srv.URL+"/v1/infer_schema", "application/json",
		strings.NewReader(`{"samples":[null]}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("null sample entries must 400, not masquerade as empty inference, got %d", res.StatusCode)
	}
}

func TestCreateTableRejectsSQLKeywordFieldAndTable(t *testing.T) {
	srv := newTestServer(t)

	// Field named "order" is a SQLite/SQL keyword and must be rejected.
	code, res := post(t, srv.URL, "create_table", map[string]any{
		"namespace": "skills",
		"table":     "findings",
		"fields": []map[string]any{
			{"name": "order", "type": "string"},
		},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("expected SQL keyword field name to be rejected, got code %d %v", code, res)
	}
	errObj := errorBody(t, res)
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "my_order") {
		t.Fatalf("expected error to suggest an alternative, got %v", res)
	}

	// Table named "select" is a SQLite/SQL keyword and must be rejected.
	code, res = post(t, srv.URL, "create_table", map[string]any{
		"namespace": "skills",
		"table":     "select",
		"fields": []map[string]any{
			{"name": "title", "type": "string"},
		},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("expected SQL keyword table name to be rejected, got code %d %v", code, res)
	}
	errObj = errorBody(t, res)
	msg, _ = errObj["message"].(string)
	if !strings.Contains(msg, "my_select") {
		t.Fatalf("expected error to suggest an alternative, got %v", res)
	}
}

func TestVersionEndpoint(t *testing.T) {
	res, err := http.Get(newTestServer(t).URL + "/version")
	if err != nil {
		t.Fatalf("get /version: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /version: got %d", res.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode /version: %v", err)
	}
	if body["version"] != version.Version {
		t.Fatalf("/version must report the injected version %q, got %v", version.Version, body["version"])
	}
	if body["name"] != "dolmen" {
		t.Fatalf("/version must report the server name, got %v", body["name"])
	}
}
