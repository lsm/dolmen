package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/auth"
	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/files"
	"github.com/lsm/dolmen/internal/store"
)

type filePart struct {
	name, filename, contentType string
	data                        []byte
}

func newFileServer(t *testing.T, cfg files.Config) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	cfg.Root = dir
	srv := httptest.NewServer(New(st, fakeEmb{}, WithFiles(files.New(cfg))).Handler())
	t.Cleanup(srv.Close)
	return srv, dir
}

func postMultipart(t *testing.T, base, op, request string, parts ...filePart) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("request", request); err != nil {
		t.Fatal(err)
	}
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="`+p.name+`"; filename="`+p.filename+`"`)
		if p.contentType != "" {
			h.Set("Content-Type", p.contentType)
		}
		w, err := mw.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(p.data)
	}
	mw.Close()
	res, err := http.Post(base+"/v1/"+op, mw.FormDataContentType(), &buf)
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

func createMeetings(t *testing.T, base string, extra map[string]any) {
	t.Helper()
	field := map[string]any{"name": "recording", "type": "file"}
	for k, v := range extra {
		field[k] = v
	}
	code, body := post(t, base, "create_table", map[string]any{"namespace": "app", "table": "meetings", "fields": []any{
		map[string]any{"name": "title", "type": "string"}, field,
	}})
	if code != http.StatusOK {
		t.Fatalf("create_table: %d %v", code, body)
	}
}

func readRow(t *testing.T, base string, id int) map[string]any {
	t.Helper()
	code, body := post(t, base, "read_rows", map[string]any{"namespace": "app", "table": "meetings", "ids": []int{id}})
	if code != http.StatusOK {
		t.Fatalf("read_rows: %d %v", code, body)
	}
	rows := body["data"].(map[string]any)["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("read_rows %d: %v", id, body)
	}
	return rows[0].(map[string]any)
}

func TestAWriteAttachesAFileInOneMultipartCall(t *testing.T) {
	srv, dir := newFileServer(t, files.Config{})
	createMeetings(t, srv.URL, nil)
	audio := []byte("OggS fake audio bytes")
	code, body := postMultipart(t, srv.URL, "insert",
		`{"namespace":"app","table":"meetings","records":[{"title":"standup","recording":{"part":"rec"}}]}`,
		filePart{name: "rec", filename: "standup.ogg", contentType: "application/octet-stream", data: audio})
	if code != http.StatusOK {
		t.Fatalf("multipart insert: %d %v", code, body)
	}
	sum := sha256.Sum256(audio)
	ref, ok := readRow(t, srv.URL, 1)["recording"].(map[string]any)
	if !ok {
		t.Fatalf("recording should read back as a reference object")
	}
	if ref["sha256"] != hex.EncodeToString(sum[:]) || ref["size"] != float64(len(audio)) || ref["media_type"] != "audio/ogg" || ref["name"] != "standup.ogg" {
		t.Fatalf("reference = %v", ref)
	}

	if got := storedFile(t, dir, "app", ref); !bytes.Equal(got, audio) {
		t.Fatalf("stored bytes = %q", got)
	}
}

func TestIdenticalBytesAreStoredOnce(t *testing.T) {
	srv, dir := newFileServer(t, files.Config{})
	createMeetings(t, srv.URL, nil)
	data := []byte("same bytes")
	for i := 0; i < 2; i++ {
		code, body := postMultipart(t, srv.URL, "insert",
			`{"namespace":"app","table":"meetings","records":[{"title":"a","recording":{"part":"f"}},{"title":"b","recording":{"part":"f","name":"copy.txt"}}]}`,
			filePart{name: "f", filename: "a.txt", data: data})
		if code != http.StatusOK {
			t.Fatalf("insert %d: %d %v", i, code, body)
		}
	}
	var blobs int
	filepath.WalkDir(filepath.Join(dir, "app.files"), func(path string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			blobs++
		}
		return nil
	})
	if blobs != 1 {
		t.Fatalf("stored %d blobs for four references to the same bytes, want 1", blobs)
	}
	if readRow(t, srv.URL, 2)["recording"].(map[string]any)["name"] != "copy.txt" {
		t.Fatalf("a per-value name should override the part's file name")
	}
	tmp, _ := os.ReadDir(filepath.Join(dir, ".files-tmp"))
	if len(tmp) != 0 {
		t.Fatalf("temporary files left behind: %v", tmp)
	}
}

func TestFileWritesTeachTheNextCall(t *testing.T) {
	srv, _ := newFileServer(t, files.Config{MaxFileSize: 64})
	createMeetings(t, srv.URL, map[string]any{"media_types": []string{"audio/*"}, "max_bytes": 16})
	cases := []struct {
		label, request string
		parts          []filePart
		status         int
		want           string
	}{
		{"typo in part name", `{"namespace":"app","table":"meetings","records":[{"recording":{"part":"recc"}}]}`,
			[]filePart{{name: "rec", filename: "a.ogg", data: []byte("x")}}, 400, `the request carries "rec"`},
		{"read-back object", `{"namespace":"app","table":"meetings","records":[{"recording":{"sha256":"ab","size":1}}]}`,
			nil, 400, "cannot be written"},
		{"wrong media type", `{"namespace":"app","table":"meetings","records":[{"recording":{"part":"rec"}}]}`,
			[]filePart{{name: "rec", filename: "a.pdf", data: []byte("x")}}, 415, "accepts audio/*"},
		{"over max_bytes", `{"namespace":"app","table":"meetings","records":[{"recording":{"part":"rec"}}]}`,
			[]filePart{{name: "rec", filename: "a.ogg", data: bytes.Repeat([]byte("x"), 32)}}, 413, "max_bytes"},
		{"over max-file-size", `{"namespace":"app","table":"meetings","records":[{"recording":{"part":"rec"}}]}`,
			[]filePart{{name: "rec", filename: "a.ogg", data: bytes.Repeat([]byte("x"), 65)}}, 413, "-max-file-size"},
	}
	for _, tc := range cases {
		code, body := postMultipart(t, srv.URL, "insert", tc.request, tc.parts...)
		if code != tc.status || errCode(body) != "invalid_request" || !strings.Contains(errMessage(body), tc.want) {
			t.Errorf("%s: %d %v, want %d invalid_request containing %q", tc.label, code, body, tc.status, tc.want)
		}
	}
	code, body := post(t, srv.URL, "insert", map[string]any{"namespace": "app", "table": "meetings", "records": []any{map[string]any{"recording": map[string]any{"part": "rec"}}}})
	if code != 400 || !strings.Contains(errMessage(body), "curl -F") {
		t.Fatalf("a part value in a JSON body should teach the multipart form: %d %v", code, body)
	}
}

func TestTheRequestPartMustComeFirst(t *testing.T) {
	srv, _ := newFileServer(t, files.Config{})
	createMeetings(t, srv.URL, nil)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("rec", "a.ogg")
	fw.Write([]byte("x"))
	mw.WriteField("request", `{"namespace":"app","table":"meetings","records":[{"recording":{"part":"rec"}}]}`)
	mw.Close()
	res, err := http.Post(srv.URL+"/v1/insert", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body map[string]any
	json.NewDecoder(res.Body).Decode(&body)
	if res.StatusCode != 400 || !strings.Contains(errMessage(body), `must be named "request"`) {
		t.Fatalf("got %d %v", res.StatusCode, body)
	}
}

func TestOnlyWritesAcceptMultipart(t *testing.T) {
	srv, _ := newFileServer(t, files.Config{})
	createMeetings(t, srv.URL, nil)
	code, body := postMultipart(t, srv.URL, "query", `{"namespace":"app","sql":"SELECT 1"}`)
	if code != http.StatusUnsupportedMediaType || !strings.Contains(errMessage(body), "accepted only by insert") {
		t.Fatalf("got %d %v", code, body)
	}
}

func TestANamespaceOverItsFilesCapRefusesTheWrite(t *testing.T) {
	srv, _ := newFileServer(t, files.Config{MaxNamespaceFiles: 10})
	createMeetings(t, srv.URL, nil)
	req := `{"namespace":"app","table":"meetings","records":[{"recording":{"part":"f"}}]}`
	if code, body := postMultipart(t, srv.URL, "insert", req, filePart{name: "f", filename: "a.bin", data: []byte("12345678")}); code != 200 {
		t.Fatalf("first file: %d %v", code, body)
	}
	code, body := postMultipart(t, srv.URL, "insert", req, filePart{name: "f", filename: "b.bin", data: []byte("abcdefgh")})
	if code != http.StatusInsufficientStorage || !strings.Contains(errMessage(body), "-max-namespace-files-size") {
		t.Fatalf("over the cap: %d %v", code, body)
	}
	if code, body := postMultipart(t, srv.URL, "insert", req, filePart{name: "f", filename: "c.bin", data: []byte("12345678")}); code != 200 {
		t.Fatalf("bytes already stored add nothing and must be accepted at the cap: %d %v", code, body)
	}
}

func TestPendingFileBytesAreBoundedServerWide(t *testing.T) {
	srv, _ := newFileServer(t, files.Config{MaxPendingFiles: 4})
	createMeetings(t, srv.URL, nil)
	code, body := postMultipart(t, srv.URL, "insert", `{"namespace":"app","table":"meetings","records":[{"recording":{"part":"f"}}]}`,
		filePart{name: "f", filename: "a.bin", data: []byte("123456789")})
	if code != http.StatusServiceUnavailable || errCode(body) != "storage_unavailable" {
		t.Fatalf("got %d %v", code, body)
	}
}

func TestAMultipartInsertReplaysByBytesNotPartNames(t *testing.T) {
	srv, _ := newFileServer(t, files.Config{})
	createMeetings(t, srv.URL, nil)
	req := `{"namespace":"app","table":"meetings","idempotency_key":"k1","records":[{"recording":{"part":"f"}}]}`
	if code, body := postMultipart(t, srv.URL, "insert", req, filePart{name: "f", filename: "a.bin", data: []byte("one")}); code != 200 {
		t.Fatalf("first: %d %v", code, body)
	}
	if code, body := postMultipart(t, srv.URL, "insert", req, filePart{name: "f", filename: "a.bin", data: []byte("one")}); code != 200 || body["data"].(map[string]any)["ids"].([]any)[0] != float64(1) {
		t.Fatalf("same bytes should replay: %d %v", code, body)
	}
	if code, body := postMultipart(t, srv.URL, "insert", req, filePart{name: "f", filename: "a.bin", data: []byte("two")}); code == 200 {
		t.Fatalf("different bytes under the same key must not replay: %d %v", code, body)
	}
}

func TestEveryWriteOperationAttachesFiles(t *testing.T) {
	srv, dir := newFileServer(t, files.Config{})
	createMeetings(t, srv.URL, nil)
	steps := []struct {
		op, request string
		data        string
		id          int
	}{
		{"insert", `{"namespace":"app","table":"meetings","records":[{"title":"a"}]}`, "", 0},
		{"update", `{"namespace":"app","table":"meetings","filter":"id = ?","args":[1],"set":{"recording":{"part":"f"}}}`, "update bytes", 1},
		{"upsert", `{"namespace":"app","table":"meetings","filter":"title = ?","args":["b"],"set":{"title":"b","recording":{"part":"f"}}}`, "upsert bytes", 2},
		{"upsert_by_key", `{"namespace":"app","table":"meetings","on":["title"],"records":[{"title":"c","recording":{"part":"f"}}]}`, "upsert_by_key bytes", 3},
		{"batch", `{"namespace":"app","writes":[{"kind":"insert","table":"meetings","records":[{"title":"d","recording":{"part":"f"}}]}]}`, "batch bytes", 4},
	}
	for _, st := range steps {
		var parts []filePart
		if st.data != "" {
			parts = append(parts, filePart{name: "f", filename: "x.txt", data: []byte(st.data)})
		}
		code, body := postMultipart(t, srv.URL, st.op, st.request, parts...)
		if code != http.StatusOK {
			t.Fatalf("%s: %d %v", st.op, code, body)
		}
		if st.id == 0 {
			continue
		}
		ref, _ := readRow(t, srv.URL, st.id)["recording"].(map[string]any)
		if got := storedFile(t, dir, "app", ref); string(got) != st.data {
			t.Fatalf("%s: row %d holds %q, want %q", st.op, st.id, got, st.data)
		}
	}
}

func storedFile(t *testing.T, dir, ns string, ref map[string]any) []byte {
	t.Helper()
	sum, _ := ref["sha256"].(string)
	if len(sum) != 64 {
		t.Fatalf("not a file reference: %v", ref)
	}
	got, err := os.ReadFile(filepath.Join(dir, ns+".files", sum[:2], sum))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestAMultipartReadErrorDoesNotEchoItsCause(t *testing.T) {
	s := New(nil, fakeEmb{})
	err := WrapError(s.partReadError(errors.New("multipart: NextPart: read tcp 10.0.0.5:8790->10.0.0.9:51234: connection reset"), "cannot read the multipart body"))
	if err.Status != http.StatusBadRequest || strings.Contains(err.Message, "10.0.0.5") || !strings.Contains(err.Message, "cannot read the multipart body") {
		t.Fatalf("got %d %q", err.Status, err.Message)
	}
}

func TestAFailedWriteLeavesNoFileBehind(t *testing.T) {
	srv, dir := newFileServer(t, files.Config{MaxNamespaceFiles: 10})
	code, body := post(t, srv.URL, "create_table", map[string]any{"namespace": "app", "table": "meetings", "fields": []any{
		map[string]any{"name": "recording", "type": "file"},
		map[string]any{"name": "count", "type": "number"},
	}})
	if code != http.StatusOK {
		t.Fatalf("create_table: %d %v", code, body)
	}
	code, body = postMultipart(t, srv.URL, "insert", `{"namespace":"app","table":"meetings","records":[{"recording":{"part":"f"},"count":"not a number"}]}`,
		filePart{name: "f", filename: "a.bin", data: []byte("12345678")})
	if code != http.StatusBadRequest {
		t.Fatalf("the write should fail on count: %d %v", code, body)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "app.files")); len(entries) != 0 {
		t.Fatalf("a failed write left %d entries in the namespace's store", len(entries))
	}
	code, body = postMultipart(t, srv.URL, "insert", `{"namespace":"app","table":"meetings","records":[{"recording":{"part":"f"},"count":1}]}`,
		filePart{name: "f", filename: "b.bin", data: []byte("abcdefgh")})
	if code != http.StatusOK {
		t.Fatalf("the failed write's file must not count toward the cap: %d %v", code, body)
	}
}

func TestDownloadAuthorizesLikeReadRows(t *testing.T) {
	srv, _ := newFileServer(t, files.Config{})
	createMeetings(t, srv.URL, nil)
	postMultipart(t, srv.URL, "insert", `{"namespace":"app","table":"meetings","records":[{"title":"x"}]}`)
	for _, tc := range []struct {
		query  string
		status int
	}{
		{"namespace=app&table=meetings&id=1&field=recording", 404},
		{"namespace=app&table=meetings&id=9&field=recording", 404},
		{"namespace=app&table=meetings&id=1&field=title", 400},
		{"namespace=app&table=meetings&id=1&field=nope", 404},
		{"namespace=app&table=meetings", 400},
	} {
		res, err := http.Get(srv.URL + "/v1/files?" + tc.query)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != tc.status {
			t.Errorf("%s: %d, want %d", tc.query, res.StatusCode, tc.status)
		}
	}
}

func TestContentDispositionKeepsTheHeaderWellFormed(t *testing.T) {
	got := contentDisposition(`naïve "plan"; v2.pdf`, "x")
	want := `attachment; filename="na_ve _plan_; v2.pdf"; filename*=UTF-8''na%C3%AFve%20%22plan%22%3B%20v2.pdf`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestAJSONValueShapedLikeAReferenceIsNotDownloadable(t *testing.T) {
	srv, _ := newFileServer(t, files.Config{})
	code, body := post(t, srv.URL, "create_table", map[string]any{"namespace": "app", "table": "notes", "fields": []any{
		map[string]any{"name": "meta", "type": "json"},
	}})
	if code != 200 {
		t.Fatalf("create_table: %d %v", code, body)
	}
	post(t, srv.URL, "insert", map[string]any{"namespace": "app", "table": "notes", "records": []any{
		map[string]any{"meta": map[string]any{"sha256": strings.Repeat("ab", 32)}},
	}})
	res, err := http.Get(srv.URL + "/v1/files?namespace=app&table=notes&id=1&field=meta")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a json field is not a file field: got %d, want 400", res.StatusCode)
	}
}

func TestADownloadServesTheBytesWithSafeHeaders(t *testing.T) {
	srv, _ := newFileServer(t, files.Config{})
	createMeetings(t, srv.URL, nil)
	audio := []byte("OggS fake audio bytes")
	if code, body := postMultipart(t, srv.URL, "insert", `{"namespace":"app","table":"meetings","records":[{"recording":{"part":"rec"}}]}`,
		filePart{name: "rec", filename: "standup.ogg", data: audio}); code != http.StatusOK {
		t.Fatalf("insert: %d %v", code, body)
	}
	sum := sha256.Sum256(audio)
	res, err := http.Get(srv.URL + "/v1/files?namespace=app&table=meetings&id=1&field=recording")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !bytes.Equal(got, audio) {
		t.Fatalf("download: %d %q", res.StatusCode, got)
	}
	h := res.Header
	if h.Get("Content-Type") != "audio/ogg" || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Content-Security-Policy") != "sandbox" || h.Get("ETag") != `"`+hex.EncodeToString(sum[:])+`"` {
		t.Fatalf("download headers: %v", h)
	}
	if !strings.HasPrefix(h.Get("Content-Disposition"), `attachment; filename="standup.ogg"`) {
		t.Fatalf("Content-Disposition = %q", h.Get("Content-Disposition"))
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/files?namespace=app&table=meetings&id=1&field=recording", nil)
	req.Header.Set("Range", "bytes=0-3")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusPartialContent || string(got) != "OggS" {
		t.Fatalf("range: %d %q", res.StatusCode, got)
	}
}

func TestDroppingANamespaceRemovesItsFiles(t *testing.T) {
	srv, dir := newFileServer(t, files.Config{MaxNamespaceFiles: 10})
	createMeetings(t, srv.URL, nil)
	req := `{"namespace":"app","table":"meetings","records":[{"recording":{"part":"f"}}]}`
	if code, body := postMultipart(t, srv.URL, "insert", req, filePart{name: "f", filename: "a.bin", data: []byte("12345678")}); code != http.StatusOK {
		t.Fatalf("insert: %d %v", code, body)
	}
	if code, body := post(t, srv.URL, "drop_namespace", map[string]any{"namespace": "app", "confirm": "app"}); code != http.StatusOK {
		t.Fatalf("drop_namespace: %d %v", code, body)
	}
	if _, err := os.Stat(filepath.Join(dir, "app.files")); !os.IsNotExist(err) {
		t.Fatalf("the namespace's files must be gone: %v", err)
	}
	createMeetings(t, srv.URL, nil)
	if code, body := postMultipart(t, srv.URL, "insert", req, filePart{name: "f", filename: "b.bin", data: []byte("abcdefgh")}); code != http.StatusOK {
		t.Fatalf("a recreated namespace must start with an empty file total: %d %v", code, body)
	}
}

func TestVacuumRemovesFilesNoRowReferences(t *testing.T) {
	srv, dir := newFileServer(t, files.Config{})
	createMeetings(t, srv.URL, nil)
	if code, body := postMultipart(t, srv.URL, "insert", `{"namespace":"app","table":"meetings","records":[{"title":"a","recording":{"part":"f"}},{"title":"b","recording":{"part":"g"}}]}`,
		filePart{name: "f", filename: "a.bin", data: []byte("keep me")}, filePart{name: "g", filename: "b.bin", data: []byte("drop")}); code != http.StatusOK {
		t.Fatalf("insert: %d %v", code, body)
	}
	dropped, _ := readRow(t, srv.URL, 2)["recording"].(map[string]any)
	if code, body := post(t, srv.URL, "update", map[string]any{"namespace": "app", "table": "meetings", "filter": "id = ?", "args": []any{2}, "set": map[string]any{"recording": nil}}); code != http.StatusOK {
		t.Fatalf("update: %d %v", code, body)
	}
	code, body := post(t, srv.URL, "vacuum", map[string]any{"namespace": "app"})
	if code != http.StatusOK {
		t.Fatalf("vacuum: %d %v", code, body)
	}
	data := body["data"].(map[string]any)
	if data["files_removed"] != float64(1) || data["file_bytes_removed"] != float64(4) || data["file_bytes"] != float64(7) {
		t.Fatalf("vacuum result = %v", data)
	}
	sum := dropped["sha256"].(string)
	if _, err := os.Stat(filepath.Join(dir, "app.files", sum[:2], sum)); !os.IsNotExist(err) {
		t.Fatalf("the unreferenced file must be gone: %v", err)
	}
	storedFile(t, dir, "app", readRow(t, srv.URL, 1)["recording"].(map[string]any))
}

func TestADownloadRacingAVacuumAnswersNotFound(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	api := New(st, fakeEmb{}, WithFiles(files.New(files.Config{Root: dir})))
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	createMeetings(t, srv.URL, nil)
	if code, body := postMultipart(t, srv.URL, "insert", `{"namespace":"app","table":"meetings","records":[{"recording":{"part":"f"}}]}`,
		filePart{name: "f", filename: "a.bin", data: []byte("racing")}); code != http.StatusOK {
		t.Fatalf("insert: %d %v", code, body)
	}
	var raced sync.Once
	api.beforeFileOpen = func() {
		raced.Do(func() {
			ctx := context.Background()
			api.Dispatch(ctx, "update", []byte(`{"namespace":"app","table":"meetings","filter":"id = ?","args":[1],"set":{"recording":null}}`))
			api.Dispatch(store.WithFileSweeper(ctx, api.files), "vacuum", []byte(`{"namespace":"app"}`))
		})
	}
	res, err := http.Get(srv.URL + "/v1/files?namespace=app&table=meetings&id=1&field=recording")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("a file removed between reading its row and opening it: got %d, want 404", res.StatusCode)
	}
}

func TestASignedDownloadIsCheckedBeforeAnyRowIsRead(t *testing.T) {
	dir := t.TempDir()
	s := &Server{files: files.New(files.Config{Root: dir})}
	sum := strings.Repeat("a", 64)
	q := url.Values{"exp": {"1000"}, "sha256": {sum}, "sig": {"00"}}
	if _, err := s.verifySignedDownload(context.Background(), q, "app", "docs", 1, "doc", time.Unix(1001, 0)); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("a link past exp must be refused as expired, got %v", err)
	}
	if _, err := s.verifySignedDownload(context.Background(), q, "app", "docs", 1, "doc", time.Unix(1000, 0)); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("a forged link must be refused, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "files.key")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("checking a forged link must not create the signing key: %v", err)
	}
	sig, err := s.files.Signer().Sign("app", "docs", 1, "doc", sum, 1000)
	if err != nil {
		t.Fatal(err)
	}
	q.Set("sig", sig)
	ctx, err := s.verifySignedDownload(context.Background(), q, "app", "docs", 1, "doc", time.Unix(1000, 0))
	if err != nil || auth.IdentityFrom(ctx).Principal != auth.AdminPrincipal {
		t.Fatalf("a valid link at its exp second must pass, got %v", err)
	}
	if _, err := s.verifySignedDownload(context.Background(), q, "app", "docs", 2, "doc", time.Unix(1000, 0)); err == nil {
		t.Fatal("a valid signature must not cover another row")
	}
}

func TestFileURLWithoutAnHTTPEndpointSaysToCallItOverHTTP(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for name, srv := range map[string]*Server{"without files": New(st, fakeEmb{}), "with files": New(st, fakeEmb{}, WithFiles(files.New(files.Config{Root: t.TempDir()})))} {
		_, err := srv.Dispatch(context.Background(), "file_url", []byte(`{"namespace":"app","table":"docs","id":1,"field":"doc"}`))
		if err == nil || !strings.Contains(err.Error(), "call file_url over HTTP") {
			t.Errorf("%s: file_url with no public base URL = %v, want the over-HTTP advice", name, err)
		}
	}
}

func TestASignedDownloadKeepsOutagesDistinctFromStaleLinks(t *testing.T) {
	ref := fileRef{sha: strings.Repeat("a", 64)}
	for name, c := range map[string]struct {
		err   error
		stale bool
	}{
		"row gone":       {notFound("table docs has no row 1"), true},
		"not a file":     {badRequest("field %q of %s is not a file field", "doc", "docs"), true},
		"engine timeout": {&Error{Status: http.StatusGatewayTimeout, Code: ErrCodeTimeout, Message: "timed out"}, false},
		"store outage":   {&files.Error{Status: http.StatusServiceUnavailable, Code: derr.StorageUnavailable, Message: "down"}, false},
	} {
		if got := linkNoLongerMatches(c.err, ref, ref.sha); got != c.stale {
			t.Errorf("%s: treated as a stale link = %v, want %v", name, got, c.stale)
		}
	}
	if !linkNoLongerMatches(nil, ref, strings.Repeat("b", 64)) || linkNoLongerMatches(nil, ref, ref.sha) {
		t.Fatal("a link matches only the row's current hash")
	}
}

func TestFileURLRefusesToQuoteAnUnusableHost(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(st, fakeEmb{}, WithFiles(files.New(files.Config{Root: t.TempDir()})))
	r := httptest.NewRequest(http.MethodPost, "/v1/file_url", nil)
	r.Host = "a;b.example.com"
	ctx := WithPublicBase(context.Background(), srv.PublicBase(r))
	_, err = srv.Dispatch(ctx, "file_url", []byte(`{"namespace":"app","table":"docs","id":1,"field":"doc"}`))
	if err == nil || !strings.Contains(err.Error(), "not a usable host name") {
		t.Fatalf("file_url behind an unusable Host must refuse rather than mint a dead link, got %v", err)
	}
}
