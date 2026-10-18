package conformance

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"testing"
)

func (h *harness) multipartNamed(request, part, filename string, data []byte) (int, map[string]any) {
	h.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("request", request)
	hdr := textproto.MIMEHeader{}
	hdr.Set("Content-Disposition", `form-data; name="`+part+`"; filename="`+filename+`"`)
	hdr.Set("Content-Type", "text/plain")
	w, _ := mw.CreatePart(hdr)
	w.Write(data)
	mw.Close()
	res, err := http.Post(h.srv.URL+"/v1/insert", mw.FormDataContentType(), &buf)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		h.t.Fatal(err)
	}
	return res.StatusCode, out
}

func TestAFileNameBreakingTheRulesIsRefusedFromAPartOrJSON(t *testing.T) {
	h := newHarness(t)
	h.seedTable("fil", "docs", fileTableFields())
	long := strings.Repeat("a", 252) + ".pdf"
	for _, tc := range []struct{ name, rule string }{
		{long, "1 to 255 bytes"},
		{".", "cannot be . or .."},
		{"..", "cannot be . or .."},
		{"evil‮fdp.exe", "bidirectional controls"},
	} {
		code, body := h.multipartNamed(`{"namespace":"fil","table":"docs","records":[{"doc":{"part":"f"}}]}`, "f", tc.name, []byte("x"))
		if code != http.StatusBadRequest || errCode(body) != "invalid_request" || !strings.Contains(errMessage(body), tc.rule) || !strings.Contains(errMessage(body), "filename=") {
			t.Errorf("part filename %q: %d %v", tc.name, code, body)
		}
		rec := map[string]any{"title": "named", "doc": map[string]any{"part": "f", "name": tc.name}}
		raw, _ := json.Marshal(map[string]any{"namespace": "fil", "table": "docs", "records": []any{rec}})
		code, body = h.multipartCall("insert", string(raw), map[string][]byte{"f": []byte("x")})
		if code != http.StatusBadRequest || errCode(body) != "invalid_request" || !strings.Contains(errMessage(body), tc.rule) {
			t.Errorf("JSON name %q: %d %v", tc.name, code, body)
		}
	}
	if rows := h.mustHTTP("read_rows", map[string]any{"namespace": "fil", "table": "docs"})["rows"].([]any); len(rows) != 0 {
		t.Fatalf("a refused name must store nothing: %v", rows)
	}
	code, body := h.multipartNamed(`{"namespace":"fil","table":"docs","records":[{"doc":{"part":"f"}}]}`, "f", "dir/notes.txt", []byte("x"))
	if code != http.StatusOK {
		t.Fatalf("a filename with directories keeps its base name: %d %v", code, body)
	}
	rows := h.mustHTTP("read_rows", map[string]any{"namespace": "fil", "table": "docs"})["rows"].([]any)
	if got := rows[0].(map[string]any)["doc"].(map[string]any)["name"]; got != "notes.txt" {
		t.Fatalf("stored name %v, want notes.txt", got)
	}
}
