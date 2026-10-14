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

	"github.com/lsm/dolmen/internal/store"
)

func fileTableFields() []map[string]any {
	return []map[string]any{
		{"name": "title", "type": "string"},
		{"name": "doc", "type": "file", "media_types": []string{"text/*", "application/pdf"}},
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

func TestFileFieldContract(t *testing.T) {
	h := newHarness(t)
	if testEngine(t) != store.EngineSQLite {
		h.ensureNS("fil")
		code, body := h.httpCall("create_table", map[string]any{"namespace": "fil", "table": "docs", "fields": fileTableFields()})
		if code == http.StatusOK || !strings.Contains(errMessage(body), "file fields are not supported on the") {
			t.Fatalf("engine %s must refuse a file field with an explanation: %d %v", testEngine(t), code, body)
		}
		return
	}
	h.seedTable("fil", "docs", fileTableFields())
	data := []byte("quarterly notes")
	code, body := h.multipartCall("insert", `{"namespace":"fil","table":"docs","records":[{"title":"q3","doc":{"part":"notes"}}]}`, map[string][]byte{"notes": data})
	if code != http.StatusOK {
		t.Fatalf("multipart insert: %d %v", code, body)
	}
	sum := sha256.Sum256(data)
	want := map[string]any{"sha256": hex.EncodeToString(sum[:]), "size": float64(len(data)), "media_type": "text/plain", "name": "notes.txt"}

	httpRow := h.mustHTTP("read_rows", map[string]any{"namespace": "fil", "table": "docs", "ids": []int{1}})["rows"].([]any)[0].(map[string]any)
	mcpRow := h.mustMCP("read_rows", map[string]any{"namespace": "fil", "table": "docs", "ids": []int{1}})["rows"].([]any)[0].(map[string]any)
	for label, row := range map[string]map[string]any{"http": httpRow, "mcp": mcpRow} {
		got, _ := json.Marshal(row["doc"])
		exp, _ := json.Marshal(want)
		if string(got) != string(exp) {
			t.Fatalf("%s read of a file field = %s, want %s", label, got, exp)
		}
	}
	queried := h.mustHTTP("query", map[string]any{"namespace": "fil", "sql": "SELECT doc FROM docs WHERE id = 1"})["rows"].([]any)[0].(map[string]any)
	if ref, ok := queried["doc"].(map[string]any); !ok || ref["sha256"] != want["sha256"] {
		t.Fatalf("query should decode a file field to its reference, got %v", queried["doc"])
	}

	res, err := http.Get(h.srv.URL + "/v1/files?namespace=fil&table=docs&id=1&field=doc")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !bytes.Equal(got, data) || res.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("download: %d %q %v", res.StatusCode, got, res.Header)
	}

	viaMCP := h.mcpCall("insert", map[string]any{"namespace": "fil", "table": "docs", "records": []any{map[string]any{"doc": map[string]any{"part": "notes"}}}})
	env := viaMCP.toolError()
	if msg, _ := env["message"].(string); env == nil || !strings.Contains(msg, "curl -F") {
		t.Fatalf("a part value over MCP must be refused with the curl form, got %v", viaMCP.result)
	}

	for label, v := range map[string]any{
		"read-back object": want,
		"raw string":       hex.EncodeToString(sum[:]),
		"url":              map[string]any{"url": "https://example.com/a.txt"},
	} {
		code, body := h.httpCall("insert", map[string]any{"namespace": "fil", "table": "docs", "records": []any{map[string]any{"doc": v}}})
		if code != http.StatusBadRequest || errCode(body) != "invalid_request" {
			t.Errorf("%s as a file value: %d %v, want 400 invalid_request", label, code, body)
		}
	}

	for _, change := range []map[string]any{
		{"op": "set_fulltext", "name": "doc", "value": true},
		{"op": "set_vectorize", "name": "doc", "value": true},
		{"op": "add_field", "field": map[string]any{"name": "att", "type": "file"}, "default": "x"},
	} {
		code, body := h.httpCall("migrate", map[string]any{"namespace": "fil", "table": "docs", "changes": []any{change}})
		if code != http.StatusBadRequest || !strings.Contains(errMessage(body), "not allowed on file fields") {
			t.Errorf("migrate %v: %d %v", change["op"], code, body)
		}
	}
	code, body = h.httpCall("upsert_by_key", map[string]any{"namespace": "fil", "table": "docs", "on": []string{"doc"}, "records": []any{map[string]any{"title": "x"}}})
	if code != http.StatusBadRequest {
		t.Fatalf("a file field cannot be a natural key: %d %v", code, body)
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
