package conformance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
)

func fileTableFields() []map[string]any {
	return []map[string]any{
		{"name": "title", "type": "string"},
		{"name": "doc", "type": "file", "max_bytes": 1024, "media_types": []string{"text/*", "application/pdf"}},
	}
}

func errCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func errMessage(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	m, _ := e["message"].(string)
	return m
}

func TestFileFieldDeclaration(t *testing.T) {
	h := newHarness(t)
	h.seedTable("fil", "docs", fileTableFields())
	for _, desc := range []map[string]any{
		h.mustHTTP("describe_table", map[string]any{"namespace": "fil", "table": "docs"}),
		h.mustMCP("describe_table", map[string]any{"namespace": "fil", "table": "docs"}),
	} {
		f := desc["table"].(map[string]any)["fields"].([]any)[1].(map[string]any)
		if f["type"] != "file" || f["max_bytes"] != float64(1024) || len(f["media_types"].([]any)) != 2 {
			t.Fatalf("describe_table must report the file field and its constraints, got %v", f)
		}
	}
	for _, change := range []map[string]any{
		{"op": "set_fulltext", "name": "doc", "value": true},
		{"op": "set_vectorize", "name": "doc", "value": true},
		{"op": "set_enum", "name": "doc", "enum": []string{"a"}},
		{"op": "add_field", "field": map[string]any{"name": "att", "type": "file"}, "default": "x"},
	} {
		code, body := h.httpCall("migrate", map[string]any{"namespace": "fil", "table": "docs", "changes": []any{change}})
		if code != http.StatusBadRequest || !strings.Contains(errMessage(body), "not allowed on file fields") {
			t.Errorf("migrate %v: %d %v", change["op"], code, body)
		}
	}
	code, body := h.httpCall("upsert_by_key", map[string]any{"namespace": "fil", "table": "docs", "on": []string{"doc"}, "records": []any{map[string]any{"title": "x"}}})
	if code != http.StatusBadRequest || errCode(body) != "invalid_request" {
		t.Fatalf("a file field cannot be a natural key: %d %v", code, body)
	}
	h.mustHTTP("migrate", map[string]any{"namespace": "fil", "table": "docs", "expected_version": 1, "changes": []any{
		map[string]any{"op": "add_field", "field": map[string]any{"name": "scan", "type": "file", "max_bytes": 2048, "media_types": []string{"application/pdf"}}},
	}})
	h.mustHTTP("list_migrations", map[string]any{"namespace": "fil", "table": "docs"})
	h.mustMCP("list_migrations", map[string]any{"namespace": "fil", "table": "docs"})
}

func TestAFileValueWithoutItsBytesTeachesMultipart(t *testing.T) {
	h := newHarness(t)
	h.seedTable("fil", "docs", fileTableFields())
	rec := map[string]any{"namespace": "fil", "table": "docs", "records": []any{map[string]any{"doc": map[string]any{"part": "notes"}}}}
	teaches := func(msg string) bool {
		for _, want := range []string{"an MCP call cannot carry file bytes", "multipart/form-data", "/v1/", "curl --form-string 'request=<json>' -F 'notes=@path'", "copy_from", "null"} {
			if !strings.Contains(msg, want) {
				return false
			}
		}
		return true
	}
	code, body := h.httpCall("insert", rec)
	if code != http.StatusBadRequest || errCode(body) != "invalid_request" || !teaches(errMessage(body)) {
		t.Fatalf("http: %d %v", code, body)
	}
	env := h.mcpCall("insert", rec).toolError()
	if msg, _ := env["message"].(string); !teaches(msg) {
		t.Fatalf("mcp: %v", env)
	}
}

func TestToolDescriptionsTellHowToWriteAFile(t *testing.T) {
	h := newHarness(t)
	var out struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(h.mcpRaw(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)), &out); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, tool := range out.Result.Tools {
		seen[tool.Name] = tool.Description
	}
	for _, op := range []string{"insert", "update", "upsert", "upsert_by_key", "batch"} {
		for _, want := range []string{"file field", "copy_from", "multipart"} {
			if !strings.Contains(seen[op], want) {
				t.Errorf("%s description must mention %q: %s", op, want, seen[op])
			}
		}
	}
	if d := seen["create_table"]; !strings.Contains(d, "secret") || !strings.Contains(d, "file") {
		t.Errorf("create_table must list the secret and file types: %s", d)
	}
}

func (h *harness) multipartCall(op, request string, parts map[string][]byte) (int, map[string]any) {
	h.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("request", request)
	for name, data := range parts {
		w, _ := mw.CreateFormFile(name, name+".txt")
		w.Write(data)
	}
	mw.Close()
	res, err := http.Post(h.srv.URL+"/v1/"+op, mw.FormDataContentType(), &buf)
	if err != nil {
		h.t.Fatalf("multipart %s: %v", op, err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		h.t.Fatalf("decode multipart %s: %v", op, err)
	}
	return res.StatusCode, out
}

func TestAMultipartWriteStoresAReferenceEveryReadAgreesOn(t *testing.T) {
	h := newHarness(t)
	fields := fileTableFields()
	fields[0]["fulltext"] = true
	fields = append(fields, map[string]any{"name": "v", "type": "vector", "dim": 2})
	h.seedTable("fil", "docs", fields)
	data := []byte("quarterly notes")
	code, body := h.multipartCall("insert", `{"namespace":"fil","table":"docs","records":[{"title":"quarterly","v":[1,0],"doc":{"part":"notes"}}]}`, map[string][]byte{"notes": data})
	if code != http.StatusOK {
		t.Fatalf("multipart insert: %d %v", code, body)
	}
	sum := sha256.Sum256(data)
	want, _ := json.Marshal(map[string]any{"sha256": hex.EncodeToString(sum[:]), "size": len(data), "media_type": "text/plain", "name": "notes.txt"})
	reads := map[string]any{
		"http":  h.mustHTTP("read_rows", map[string]any{"namespace": "fil", "table": "docs", "ids": []int{1}})["rows"].([]any)[0].(map[string]any)["doc"],
		"mcp":   h.mustMCP("read_rows", map[string]any{"namespace": "fil", "table": "docs", "ids": []int{1}})["rows"].([]any)[0].(map[string]any)["doc"],
		"query": h.mustHTTP("query", map[string]any{"namespace": "fil", "sql": "SELECT doc FROM docs WHERE id = 1"})["rows"].([]any)[0].(map[string]any)["doc"],
	}
	for transport, call := range map[string]func(string, any) map[string]any{"http": h.mustHTTP, "mcp": h.mustMCP} {
		for op, input := range map[string]map[string]any{
			"search_fulltext": {"namespace": "fil", "table": "docs", "query": "quarterly"},
			"search_vector":   {"namespace": "fil", "table": "docs", "column": "v", "vector": []any{1, 0}},
		} {
			results := call(op, input)["results"].([]any)
			if len(results) != 1 {
				t.Fatalf("%s %s: %v", transport, op, results)
			}
			reads[transport+op] = results[0].(map[string]any)["doc"]
		}
	}
	for label, v := range reads {
		if got, _ := json.Marshal(v); string(got) != string(want) {
			t.Fatalf("%s read of a file field = %s, want %s", label, got, want)
		}
	}
}

func TestAFileDownloadsByRowWithReadRowsAccess(t *testing.T) {
	h := newHarness(t)
	h.seedTable("fil", "docs", fileTableFields())
	data := []byte("quarterly notes")
	if code, body := h.multipartCall("insert", `{"namespace":"fil","table":"docs","records":[{"title":"q3","doc":{"part":"notes"}}]}`, map[string][]byte{"notes": data}); code != http.StatusOK {
		t.Fatalf("multipart insert: %d %v", code, body)
	}
	res, err := http.Get(h.srv.URL + "/v1/files?namespace=fil&table=docs&id=1&field=doc")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !bytes.Equal(got, data) || res.Header.Get("X-Content-Type-Options") != "nosniff" || res.Header.Get("Content-Type") != "text/plain" || res.Header.Get("Content-Security-Policy") != "sandbox" || !strings.HasPrefix(res.Header.Get("Content-Disposition"), "attachment;") {
		t.Fatalf("download: %d %q %v", res.StatusCode, got, res.Header)
	}
	for query, status := range map[string]int{
		"namespace=fil&table=docs&id=2&field=doc":   http.StatusNotFound,
		"namespace=fil&table=docs&id=1&field=title": http.StatusBadRequest,
		"namespace=nope&table=docs&id=1&field=doc":  http.StatusNotFound,
	} {
		res, err := http.Get(h.srv.URL + "/v1/files?" + query)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != status {
			t.Errorf("%s: %d, want %d", query, res.StatusCode, status)
		}
	}
}

func TestSetFileConstraints(t *testing.T) {
	h := newHarness(t)
	h.seedTable("fil", "docs", fileTableFields())
	if code, body := h.multipartCall("insert", `{"namespace":"fil","table":"docs","records":[{"title":"a","doc":{"part":"f"}}]}`, map[string][]byte{"f": bytes.Repeat([]byte("x"), 600)}); code != http.StatusOK {
		t.Fatalf("insert: %d %v", code, body)
	}
	migrate := func(change map[string]any) (int, map[string]any) {
		change["op"] = "set_file_constraints"
		change["name"] = "doc"
		return h.httpCall("migrate", map[string]any{"namespace": "fil", "table": "docs", "changes": []any{change}})
	}
	if code, body := migrate(map[string]any{"max_bytes": 100}); code != http.StatusBadRequest || !strings.Contains(errMessage(body), "1 rows hold files that break them (ids 1)") {
		t.Fatalf("tightening past a stored file: %d %v", code, body)
	}
	if code, body := migrate(map[string]any{"media_types": []string{"image/*"}}); code != http.StatusBadRequest || !strings.Contains(errMessage(body), "ids 1") {
		t.Fatalf("excluding a stored file's type: %d %v", code, body)
	}
	if code, body := migrate(map[string]any{}); code != http.StatusBadRequest || !strings.Contains(errMessage(body), "needs max_bytes") {
		t.Fatalf("no constraint given: %d %v", code, body)
	}
	if code, body := migrate(map[string]any{"max_bytes": 700, "media_types": []string{}}); code != http.StatusOK {
		t.Fatalf("loosening: %d %v", code, body)
	}
	f := h.mustMCP("describe_table", map[string]any{"namespace": "fil", "table": "docs"})["table"].(map[string]any)["fields"].([]any)[1].(map[string]any)
	if f["max_bytes"] != float64(700) || f["media_types"] != nil {
		t.Fatalf("describe_table after set_file_constraints = %v", f)
	}
	h.mustHTTP("list_migrations", map[string]any{"namespace": "fil", "table": "docs"})
	h.mustMCP("list_migrations", map[string]any{"namespace": "fil", "table": "docs"})
	if code, body := h.multipartCall("insert", `{"namespace":"fil","table":"docs","records":[{"doc":{"part":"f"}}]}`, map[string][]byte{"f": bytes.Repeat([]byte("y"), 701)}); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("the new max_bytes must apply to the next write: %d %v", code, body)
	}
	if code, body := h.httpCall("migrate", map[string]any{"namespace": "fil", "table": "docs", "changes": []any{map[string]any{"op": "set_file_constraints", "name": "title", "max_bytes": 5}}}); code != http.StatusBadRequest || !strings.Contains(errMessage(body), "only allowed on file fields") {
		t.Fatalf("on a string field: %d %v", code, body)
	}
}

func TestVacuumReportsTheFilesItRemoved(t *testing.T) {
	h := newHarness(t)
	h.seedTable("fil", "docs", fileTableFields())
	if code, body := h.multipartCall("insert", `{"namespace":"fil","table":"docs","records":[{"title":"q3","doc":{"part":"notes"}}]}`, map[string][]byte{"notes": []byte("quarterly notes")}); code != http.StatusOK {
		t.Fatalf("multipart insert: %d %v", code, body)
	}
	h.mustHTTP("delete", map[string]any{"namespace": "fil", "table": "docs", "filter": "id = ?", "args": []any{1}})
	res := h.mustHTTP("vacuum", map[string]any{"namespace": "fil"})
	if res["files_removed"] != float64(1) || res["file_bytes"] != float64(0) {
		t.Fatalf("http vacuum = %v", res)
	}
	again := h.mustMCP("vacuum", map[string]any{"namespace": "fil"})
	if again["files_removed"] != float64(0) {
		t.Fatalf("mcp vacuum = %v", again)
	}
}

func (h *harness) multipartAs(t *testing.T, principal, op, request string, parts map[string][]byte) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("request", request)
	for name, data := range parts {
		w, _ := mw.CreateFormFile(name, name+".txt")
		w.Write(data)
	}
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, h.httpURL+"/"+op, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Dolmen-Principal", principal)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestCopyFromReusesAFileWithoutReuploading(t *testing.T) {
	h := newHarness(t)
	h.seedTable("fil", "docs", fileTableFields())
	if code, body := h.multipartCall("insert", `{"namespace":"fil","table":"docs","records":[{"title":"src","doc":{"part":"f"}}]}`, map[string][]byte{"f": []byte("shared notes")}); code != http.StatusOK {
		t.Fatalf("insert: %d %v", code, body)
	}
	src := h.mustHTTP("read_rows", map[string]any{"namespace": "fil", "table": "docs", "ids": []int{1}})["rows"].([]any)[0].(map[string]any)["doc"].(map[string]any)
	copyRec := map[string]any{"namespace": "fil", "table": "docs", "records": []any{map[string]any{"doc": map[string]any{"copy_from": map[string]any{"table": "docs", "id": 1, "field": "doc"}, "name": "copy.txt"}}}}
	h.mustHTTP("insert", copyRec)
	h.mustMCP("insert", copyRec)
	for _, id := range []int{2, 3} {
		ref := h.mustHTTP("read_rows", map[string]any{"namespace": "fil", "table": "docs", "ids": []int{id}})["rows"].([]any)[0].(map[string]any)["doc"].(map[string]any)
		if ref["sha256"] != src["sha256"] || ref["name"] != "copy.txt" {
			t.Fatalf("row %d copied reference = %v", id, ref)
		}
	}
	missing := map[string]any{"namespace": "fil", "table": "docs", "records": []any{map[string]any{"doc": map[string]any{"copy_from": map[string]any{"table": "docs", "id": 99, "field": "doc"}}}}}
	if code, body := h.httpCall("insert", missing); code != http.StatusBadRequest || !strings.Contains(errMessage(body), "copy_from names no file you can read") || !strings.Contains(errMessage(body), "read access to table docs") {
		t.Fatalf("copy from a missing row: %d %v", code, body)
	}
}

func TestCopyFromNeverReachesAnotherPrincipalsRow(t *testing.T) {
	h := newHarnessMode(t, authGateway)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	h.mustHTTP("create_table", map[string]any{"namespace": "acme", "table": "docs", "row_access": "own",
		"fields": []map[string]any{{"name": "doc", "type": "file"}}})
	grantTo(t, h, "principal", "alice", "acme", "docs", "create")
	grantTo(t, h, "principal", "bob", "acme", "docs", "create")
	if code, body := h.multipartAs(t, "alice", "insert", `{"namespace":"acme","table":"docs","records":[{"doc":{"part":"f"}}]}`, map[string][]byte{"f": []byte("alice's secret file")}); code != http.StatusOK {
		t.Fatalf("alice insert: %d %v", code, body)
	}
	res, out := h.asIdentity(t, "bob", "", "insert", `{"namespace":"acme","table":"docs","records":[{"doc":{"copy_from":{"table":"docs","id":1,"field":"doc"}}}]}`)
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(errMessage(out), "copy_from names no file you can read") || !strings.Contains(errMessage(out), "read access to table docs") {
		t.Fatalf("bob copying alice's file: %d %v", res.StatusCode, out)
	}
	res, out = h.asIdentity(t, "alice", "", "insert", `{"namespace":"acme","table":"docs","records":[{"doc":{"copy_from":{"table":"docs","id":1,"field":"doc"}}}]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("alice copying her own file: %d %v", res.StatusCode, out)
	}
}

func TestAURLValueTeachesTheAlternativeWhenFetchingIsOff(t *testing.T) {
	h := newHarness(t)
	h.seedTable("fil", "docs", fileTableFields())
	rec := map[string]any{"namespace": "fil", "table": "docs", "records": []any{map[string]any{"doc": map[string]any{"url": "https://files.example/a.txt"}}}}
	code, body := h.httpCall("insert", rec)
	if code != http.StatusBadRequest || errCode(body) != "invalid_request" || !strings.Contains(errMessage(body), "does not fetch URLs") {
		t.Fatalf("http: %d %v", code, body)
	}
	env := h.mcpCall("insert", rec).toolError()
	if msg, _ := env["message"].(string); !strings.Contains(msg, "-files-fetch-hosts") || !strings.Contains(msg, "ask the operator") || !strings.Contains(msg, "an MCP call cannot carry file bytes") {
		t.Fatalf("mcp: %v", env)
	}
}

func TestWritingBackAReadReferenceTeachesCopyFrom(t *testing.T) {
	h := newHarness(t)
	h.seedTable("fil", "docs", fileTableFields())
	if code, body := h.multipartCall("insert", `{"namespace":"fil","table":"docs","records":[{"doc":{"part":"f"}}]}`, map[string][]byte{"f": []byte("kept")}); code != http.StatusOK {
		t.Fatalf("insert: %d %v", code, body)
	}
	ref := h.mustHTTP("read_rows", map[string]any{"namespace": "fil", "table": "docs", "ids": []int{1}})["rows"].([]any)[0].(map[string]any)["doc"]
	rec := map[string]any{"namespace": "fil", "table": "docs", "records": []any{map[string]any{"doc": ref}}}
	code, body := h.httpCall("insert", rec)
	if code != http.StatusBadRequest || !strings.Contains(errMessage(body), `{"copy_from": {"table": "<table>", "id": <id>, "field": "<file field>"}}`) {
		t.Fatalf("http: %d %v", code, body)
	}
	if msg, _ := h.mcpCall("insert", rec).toolError()["message"].(string); !strings.Contains(msg, "copy_from") {
		t.Fatalf("mcp: %v", msg)
	}
}
