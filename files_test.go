package dolmen

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openWithDocs(t *testing.T) (*Store, string, context.Context) {
	t.Helper()
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "app", "docs", []Field{
		{Name: "title", Type: String},
		{Name: "doc", Type: File, MediaTypes: []string{"text/*"}},
		{Name: "count", Type: Number},
	}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	return st, dir, ctx
}

func readFile(t *testing.T, st *Store, ctx context.Context, id int64) ([]byte, FileInfo) {
	t.Helper()
	f, info, err := st.OpenFile(ctx, "app", "docs", id, "doc")
	if err != nil {
		t.Fatalf("OpenFile %d: %v", id, err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return got, info
}

func TestTheLibraryWritesAndOpensFiles(t *testing.T) {
	st, _, ctx := openWithDocs(t)
	res, err := st.Insert(ctx, "app", "docs", []map[string]any{
		{"title": "a", "doc": FileUpload{Reader: strings.NewReader("hello"), Name: "dir/notes.txt"}},
	}, InsertOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, info := readFile(t, st, ctx, res.Ids[0])
	if string(got) != "hello" || info.Size != 5 || info.MediaType != "text/plain" || info.Name != "notes.txt" || len(info.SHA256) != 64 {
		t.Fatalf("OpenFile = %q %+v", got, info)
	}
	if _, err := st.Update(ctx, "app", "docs", UpdateOptions{Filter: "id = ?", Args: []any{res.Ids[0]}, Set: map[string]any{"doc": &FileUpload{Reader: strings.NewReader("updated"), MediaType: "text/markdown"}}}); err != nil {
		t.Fatal(err)
	}
	if got, info := readFile(t, st, ctx, res.Ids[0]); string(got) != "updated" || info.MediaType != "text/markdown" {
		t.Fatalf("after update: %q %+v", got, info)
	}
	b, err := st.Batch(ctx, "app", []BatchWrite{
		{Kind: BatchInsert, Table: "docs", Records: []map[string]any{{"title": "b", "doc": FileUpload{Reader: strings.NewReader("batched"), Name: "b.txt"}}}},
	}, BatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := readFile(t, st, ctx, b.Results[0].Ids[0]); string(got) != "batched" {
		t.Fatalf("batch file = %q", got)
	}
}

func TestTheLibraryRefusesFilesItCannotStore(t *testing.T) {
	st, dir, ctx := openWithDocs(t)
	for label, rec := range map[string]map[string]any{
		"no reader":  {"doc": FileUpload{Name: "a.txt"}},
		"wrong type": {"doc": FileUpload{Reader: strings.NewReader("%PDF"), Name: "a.pdf"}},
		"later bad":  {"doc": FileUpload{Reader: strings.NewReader("orphan"), Name: "a.txt"}, "count": "NaN?"},
	} {
		if _, err := st.Insert(ctx, "app", "docs", []map[string]any{rec}, InsertOptions{}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v, want invalid request", label, err)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "app.files")); len(entries) != 0 {
		t.Fatalf("a failed write left %d entries in the namespace's files", len(entries))
	}
	st.Insert(ctx, "app", "docs", []map[string]any{{"title": "x"}}, InsertOptions{})
	if _, _, err := st.OpenFile(ctx, "app", "docs", 1, "title"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("OpenFile on a string field: %v", err)
	}
	if _, _, err := st.OpenFile(ctx, "app", "docs", 1, "doc"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("OpenFile on an empty file field: %v", err)
	}
}

func TestTheCallersRecordsAreNotRewritten(t *testing.T) {
	st, _, ctx := openWithDocs(t)
	up := FileUpload{Reader: bytes.NewReader([]byte("x")), Name: "a.txt"}
	rec := map[string]any{"doc": up}
	if _, err := st.Insert(ctx, "app", "docs", []map[string]any{rec}, InsertOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec["doc"].(FileUpload); !ok {
		t.Fatalf("Insert rewrote the caller's record: %v", rec)
	}
}

func TestOpenFileClassifiesAMissingFile(t *testing.T) {
	st, dir, ctx := openWithDocs(t)
	res, err := st.Insert(ctx, "app", "docs", []map[string]any{{"doc": FileUpload{Reader: strings.NewReader("gone"), Name: "a.txt"}}}, InsertOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, info := readFile(t, st, ctx, res.Ids[0])
	if err := os.Remove(filepath.Join(dir, "app.files", info.SHA256[:2], info.SHA256)); err != nil {
		t.Fatal(err)
	}
	_, _, err = st.OpenFile(ctx, "app", "docs", res.Ids[0], "doc")
	if !errors.Is(err, ErrInternal) || strings.Contains(err.Error(), dir) {
		t.Fatalf("a missing file must be ErrInternal without the path, got %v", err)
	}
}

func TestAPartMapCannotCaptureAnotherValuesUpload(t *testing.T) {
	st, _, ctx := openWithDocs(t)
	_, err := st.Insert(ctx, "app", "docs", []map[string]any{
		{"doc": FileUpload{Reader: strings.NewReader("mine"), Name: "a.txt"}},
		{"doc": map[string]any{"part": "file1"}},
	}, InsertOptions{})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a part map written through the library must be refused, got %v", err)
	}
}

type panickyReader struct{ sent bool }

func (p *panickyReader) Read(b []byte) (int, error) {
	if !p.sent {
		p.sent = true
		return copy(b, "partial"), nil
	}
	panic("reader exploded")
}

func TestAPanickingReaderLeavesNoTemporaryFile(t *testing.T) {
	st, dir, ctx := openWithDocs(t)
	func() {
		defer func() { recover() }()
		st.Insert(ctx, "app", "docs", []map[string]any{{"doc": FileUpload{Reader: &panickyReader{}, Name: "a.txt"}}}, InsertOptions{})
	}()
	if entries, _ := os.ReadDir(filepath.Join(dir, ".files-tmp")); len(entries) != 0 {
		t.Fatalf("a panicking reader left %d temporary files", len(entries))
	}
}

func TestDroppingANamespaceInTheLibraryRemovesItsFiles(t *testing.T) {
	st, dir, ctx := openWithDocs(t)
	if _, err := st.Insert(ctx, "app", "docs", []map[string]any{{"doc": FileUpload{Reader: strings.NewReader("x"), Name: "a.txt"}}}, InsertOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := st.DropNamespace(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "app.files")); !os.IsNotExist(err) {
		t.Fatalf("the namespace's files must be gone: %v", err)
	}
}

func TestOpenFileClassifiesLikeTheDownload(t *testing.T) {
	st, _, ctx := openWithDocs(t)
	if _, _, err := st.OpenFile(ctx, "app", "docs", 1, "title"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing row: %v, want not found", err)
	}
	if _, err := st.Insert(ctx, "app", "docs", []map[string]any{{"title": "x"}}, InsertOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.OpenFile(ctx, "app", "docs", 1, "titel"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a field the table does not have: %v, want not found", err)
	}
	if _, _, err := st.OpenFile(ctx, "app", "docs", 1, "title"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a field that is not a file field: %v, want invalid request", err)
	}
}

func TestTheLibraryCopiesAFileFromAnotherRow(t *testing.T) {
	st, _, ctx := openWithDocs(t)
	res, err := st.Insert(ctx, "app", "docs", []map[string]any{
		{"title": "a", "doc": FileUpload{Reader: strings.NewReader("shared"), Name: "a.txt"}},
	}, InsertOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cp, err := st.Insert(ctx, "app", "docs", []map[string]any{
		{"title": "b", "doc": FileCopy{Table: "Docs", ID: res.Ids[0], Field: "doc", Name: "b.txt"}},
		{"title": "c", "doc": &FileCopy{Table: "docs", ID: res.Ids[0], Field: "doc", MediaType: "text/markdown"}},
	}, InsertOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, info := readFile(t, st, ctx, cp.Ids[0]); string(got) != "shared" || info.Name != "b.txt" || info.MediaType != "text/plain" {
		t.Fatalf("copied with a name = %q %+v", got, info)
	}
	if got, info := readFile(t, st, ctx, cp.Ids[1]); string(got) != "shared" || info.Name != "a.txt" || info.MediaType != "text/markdown" {
		t.Fatalf("copied with a media type = %q %+v", got, info)
	}
	if _, err := st.Insert(ctx, "app", "docs", []map[string]any{{"doc": FileCopy{Table: "docs", ID: 99, Field: "doc"}}}, InsertOptions{}); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "copy_from names no file you can read") {
		t.Fatalf("copy from a missing row: %v", err)
	}
}

func TestTheLibraryFetchesOnlyWithWithFileFetch(t *testing.T) {
	st, _, ctx := openWithDocs(t)
	_, err := st.Insert(ctx, "app", "docs", []map[string]any{{"doc": FileURL{URL: "https://example.com/a.txt"}}}, InsertOptions{})
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "WithFileFetch") {
		t.Fatalf("FileURL without WithFileFetch: %v", err)
	}
	if _, err := Open(t.TempDir(), WithFileFetch([]string{"example.com"}, time.Millisecond)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a fetch timeout under a second: %v", err)
	}
	fs, err := Open(t.TempDir(), WithFileFetch([]string{"127.0.0.1"}, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()
	if _, err := fs.CreateTable(ctx, "app", "docs", []Field{{Name: "doc", Type: File}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "x") }))
	defer srv.Close()
	for url, want := range map[string]string{
		srv.URL + "/a.txt":          "not public",
		"https://example.com/a.txt": "not in the hosts given to WithFileFetch",
		"ftp://127.0.0.1/a.txt":     "dolmen.FileUpload",
	} {
		_, err := fs.Insert(ctx, "app", "docs", []map[string]any{{"doc": &FileURL{URL: url}}}, InsertOptions{})
		if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "-files-") || strings.Contains(err.Error(), "multipart") {
			t.Errorf("fetch %s: %v, want %q in library wording", url, err, want)
		}
	}
	blank, err := Open(t.TempDir(), WithFileFetch([]string{" ", ""}, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer blank.Close()
	if _, err := blank.CreateTable(ctx, "app", "docs", []Field{{Name: "doc", Type: File}}); err != nil {
		t.Fatal(err)
	}
	if _, err := blank.Insert(ctx, "app", "docs", []map[string]any{{"doc": FileURL{URL: "https://example.com/a.txt"}}}, InsertOptions{}); err == nil || !strings.Contains(err.Error(), "WithFileFetch(hosts") {
		t.Fatalf("FileURL with only blank hosts: %v", err)
	}
}
