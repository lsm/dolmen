package conformance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestFileWritesAcrossOperationsAndReplay(t *testing.T) {
	h := newHarness(t)
	h.seedTable("fil", "docs", fileTableFields())
	data := []byte("one blob shared by every write")
	steps := []struct {
		op, body string
		id       int
	}{
		{"insert", `{"namespace":"fil","table":"docs","records":[{"title":"a","doc":{"part":"f"}}]}`, 1},
		{"update", `{"namespace":"fil","table":"docs","filter":"id = 1","set":{"doc":{"part":"f"}}}`, 1},
		{"upsert", `{"namespace":"fil","table":"docs","filter":"id = 1","set":{"doc":{"part":"f"}}}`, 1},
		{"upsert", `{"namespace":"fil","table":"docs","filter":"title = 'b'","set":{"title":"b","doc":{"part":"f"}}}`, 2},
		{"upsert_by_key", `{"namespace":"fil","table":"docs","on":["title"],"records":[{"title":"b","doc":{"part":"f"}}]}`, 2},
		{"upsert_by_key", `{"namespace":"fil","table":"docs","on":["title"],"records":[{"title":"c","doc":{"part":"f"}}]}`, 3},
		{"batch", `{"namespace":"fil","writes":[{"kind":"insert","table":"docs","records":[{"title":"d","doc":{"part":"f"}}]},{"kind":"update","table":"docs","filter":"id = 1","set":{"doc":{"part":"f"}}},{"kind":"upsert","table":"docs","filter":"id = 2","set":{"doc":{"part":"f"}}},{"kind":"upsert_by_key","table":"docs","on":["title"],"records":[{"title":"c","doc":{"part":"f"}}]}]}`, 4},
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	for _, step := range steps {
		code, body := h.multipartCall(step.op, step.body, map[string][]byte{"f": data})
		if code != http.StatusOK {
			t.Fatalf("%s: %d %v", step.op, code, body)
		}
		rows := h.mustMCP("read_rows", map[string]any{"namespace": "fil", "table": "docs", "ids": []int{step.id}})["rows"].([]any)
		ref := rows[0].(map[string]any)["doc"].(map[string]any)
		if ref["sha256"] != hash {
			t.Fatalf("%s stored %v", step.op, ref)
		}
	}
	vacuum := h.mustHTTP("vacuum", map[string]any{"namespace": "fil"})
	if n, _ := toFloat(vacuum["file_bytes"]); n != float64(len(data)) {
		t.Fatalf("identical files must occupy one blob: %v", vacuum)
	}
	for _, op := range []string{"insert", "batch"} {
		request := func(part string) string {
			rec := map[string]any{"title": "replay", "doc": map[string]any{"part": part, "name": "stable.txt"}}
			body := map[string]any{"namespace": "fil", "idempotency_key": "file-" + op}
			if op == "insert" {
				body["table"] = "docs"
				body["records"] = []any{rec}
			} else {
				body["writes"] = []any{map[string]any{"kind": "insert", "table": "docs", "records": []any{rec}}}
			}
			raw, _ := json.Marshal(body)
			return string(raw)
		}
		if code, body := h.multipartCall(op, request("first"), map[string][]byte{"first": data}); code != http.StatusOK {
			t.Fatalf("%s initial: %d %v", op, code, body)
		}
		code, body := h.multipartCall(op, request("renamed"), map[string][]byte{"renamed": data})
		if code != http.StatusOK || body["data"].(map[string]any)["replayed"] != true {
			t.Fatalf("%s replay: %d %v", op, code, body)
		}
		if code, body := h.multipartCall(op, request("first"), map[string][]byte{"first": []byte("different bytes")}); code != http.StatusBadRequest || errCode(body) != "conflict" {
			t.Fatalf("%s different bytes: %d %v", op, code, body)
		}
	}
	base := h.srv.URL + "/v1/files?namespace=fil&table=docs&id=1&field=doc"
	for _, tc := range []struct {
		method, header, value string
		status                int
		want                  []byte
	}{
		{"GET", "Range", "bytes=0-3", http.StatusPartialContent, data[:4]},
		{"GET", "If-None-Match", `"` + hash + `"`, http.StatusNotModified, nil},
		{"HEAD", "", "", http.StatusOK, nil},
	} {
		req, _ := http.NewRequest(tc.method, base, nil)
		if tc.header != "" {
			req.Header.Set(tc.header, tc.value)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != tc.status || !bytes.Equal(got, tc.want) || res.Header.Get("ETag") != `"`+hash+`"` {
			t.Fatalf("%s %s: %d %q %v", tc.method, tc.header, res.StatusCode, got, res.Header)
		}
	}
	if !h.blobStored("fil", hash) {
		t.Fatal("the attached file is not in the file store")
	}
	h.mustHTTP("drop_namespace", map[string]any{"namespace": "fil", "confirm": "fil"})
	if h.blobStored("fil", hash) {
		t.Fatal("drop_namespace left the namespace's files in the file store")
	}
}

func (h *harness) blobStored(ns, sum string) bool {
	h.t.Helper()
	f, err := h.fileStore.Open(context.Background(), ns, sum)
	if err != nil {
		return false
	}
	f.Close()
	return true
}

func TestFileBatchRollbackLeavesNoBlob(t *testing.T) {
	h := newHarness(t)
	h.seedTable("fil", "docs", fileTableFields())
	data := []byte("a failed batch's file")
	body := `{"namespace":"fil","writes":[{"kind":"insert","table":"docs","records":[{"doc":{"part":"f"}}]},{"kind":"insert","table":"docs","records":[{"unknown":"field"}]}]}`
	if code, out := h.multipartCall("batch", body, map[string][]byte{"f": data}); code != http.StatusBadRequest {
		t.Fatalf("bad batch: %d %v", code, out)
	}
	rows := h.mustHTTP("read_rows", map[string]any{"namespace": "fil", "table": "docs"})["rows"].([]any)
	if len(rows) != 0 {
		t.Fatalf("failed batch committed rows: %v", rows)
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if !h.fileStore.Remote() {
		if h.blobStored("fil", hash) {
			t.Fatal("a failed batch on the local store left its blob")
		}
		return
	}
	if !h.blobStored("fil", hash) {
		t.Fatal("on a shared store a failed batch leaves its blob for the sweep, since another server may have attached it")
	}
	if res := h.mustHTTP("vacuum", map[string]any{"namespace": "fil"}); res["files_removed"] != float64(1) || h.blobStored("fil", hash) {
		t.Fatalf("vacuum must remove the failed batch's unreferenced blob: %v", res)
	}
}

func TestFileWriteMatchingNoRowStoresNoBlob(t *testing.T) {
	h := newHarness(t)
	h.seedTable("fil", "docs", fileTableFields())
	h.mustHTTP("insert", map[string]any{"namespace": "fil", "table": "docs", "records": []any{map[string]any{"title": "kept"}}})
	for _, tc := range []struct{ op, body string }{
		{"update", `{"namespace":"fil","table":"docs","filter":"id = 999","set":{"doc":{"part":"f"}}}`},
		{"batch", `{"namespace":"fil","writes":[{"kind":"update","table":"docs","filter":"id = 999","set":{"doc":{"part":"f"}}}]}`},
	} {
		data := []byte("a file no row takes, via " + tc.op)
		if code, out := h.multipartCall(tc.op, tc.body, map[string][]byte{"f": data}); code != http.StatusOK {
			t.Fatalf("%s matching no row: %d %v", tc.op, code, out)
		}
		sum := sha256.Sum256(data)
		if h.blobStored("fil", hex.EncodeToString(sum[:])) {
			t.Fatalf("a %s matching no row stored its file, which only a vacuum would remove", tc.op)
		}
	}
	if res := h.mustHTTP("vacuum", map[string]any{"namespace": "fil"}); res["files_removed"] != float64(0) {
		t.Fatalf("vacuum after writes that matched no row = %v, want nothing to remove", res)
	}
}

func TestAFileWriteMatchingNoRowIsStillChecked(t *testing.T) {
	h := newHarness(t)
	h.seedTable("fil", "docs", fileTableFields())
	big := make([]byte, 2048)
	code, out := h.multipartCall("update", `{"namespace":"fil","table":"docs","filter":"id = 999","set":{"doc":{"part":"f"}}}`, map[string][]byte{"f": big})
	if code != http.StatusRequestEntityTooLarge || !strings.Contains(errMessage(out), "max_bytes") {
		t.Fatalf("an oversized file must be refused even when no row matches: %d %v", code, out)
	}
	code, out = h.httpCall("update", map[string]any{"namespace": "fil", "table": "docs", "filter": "id = 999", "set": map[string]any{"doc": map[string]any{"copy_from": map[string]any{"table": "docs", "id": 5, "field": "doc"}}}})
	if code == http.StatusOK {
		t.Fatalf("a copy_from naming no readable file must be refused even when no row matches: %d %v", code, out)
	}
}
