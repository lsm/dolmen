package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

type fakeFiles struct {
	parts    map[string]FileBlob
	attached []string
}

func (f *fakeFiles) Describe(part string) (FileBlob, error) {
	b, ok := f.parts[part]
	if !ok {
		return FileBlob{}, invalidf("no part %q", part)
	}
	return b, nil
}

func (f *fakeFiles) Attach(_ context.Context, ns, part string) (FileBlob, error) {
	f.attached = append(f.attached, ns+":"+part)
	return f.Describe(part)
}

func fileTable(t *testing.T) (*Store, context.Context) {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTable(ctx, "app", "docs", []schema.Field{
		{Name: "title", Type: schema.String},
		{Name: "doc", Type: schema.File, MaxBytes: 100, MediaTypes: []string{"text/*"}},
	}, TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	return st, ctx
}

func blob(sum string, size int64) FileBlob {
	return FileBlob{SHA256: strings.Repeat(sum, 64), Size: size, MediaType: "text/plain", Name: "a.txt"}
}

func TestAFileValueStoresAReference(t *testing.T) {
	st, ctx := fileTable(t)
	src := &fakeFiles{parts: map[string]FileBlob{"f": blob("a", 5)}}
	res, err := st.Insert(WithFileSource(ctx, src), "app", "docs", []map[string]any{{"doc": map[string]any{"part": "f", "name": "b.txt"}}}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	if len(src.attached) != 1 || src.attached[0] != "app:f" {
		t.Fatalf("attached = %v", src.attached)
	}
	got, err := st.GetRows(ctx, "app", "docs", res.Ids, nil, Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := got.Rows[0]["doc"].(map[string]any)
	if ref["sha256"] != strings.Repeat("a", 64) || ref["media_type"] != "text/plain" || ref["name"] != "b.txt" {
		t.Fatalf("reference = %v", got.Rows[0]["doc"])
	}
}

func TestFileValuesThatCannotBeWritten(t *testing.T) {
	st, ctx := fileTable(t)
	src := &fakeFiles{parts: map[string]FileBlob{"big": blob("b", 200), "pdf": {SHA256: strings.Repeat("c", 64), Size: 1, MediaType: "application/pdf"}}}
	for _, tc := range []struct {
		label string
		ctx   context.Context
		value any
		want  string
		limit int
	}{
		{"no source", ctx, map[string]any{"part": "f"}, "curl -F", 0},
		{"read-back object", WithFileSource(ctx, src), map[string]any{"sha256": "x", "size": 1}, "cannot be written", 0},
		{"plain string", WithFileSource(ctx, src), "abc", "is a file field", 0},
		{"url", WithFileSource(ctx, src), map[string]any{"url": "https://x"}, "not supported", 0},
		{"unknown key", WithFileSource(ctx, src), map[string]any{"part": "big", "size": 1}, "unknown key", 0},
		{"bad name", WithFileSource(ctx, src), map[string]any{"part": "big", "name": "a/b"}, "file name", 0},
		{"over max_bytes", WithFileSource(ctx, src), map[string]any{"part": "big"}, "max_bytes", 413},
		{"wrong type", WithFileSource(ctx, src), map[string]any{"part": "pdf"}, "accepts text/*", 415},
	} {
		_, err := st.Insert(tc.ctx, "app", "docs", []map[string]any{{"doc": tc.value}}, WriteOpts{}, Embedder{}, nil, Incarnation{})
		if err == nil || !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an invalid-request error containing %q", tc.label, err, tc.want)
			continue
		}
		var le *FileLimitError
		if tc.limit != 0 && (!errors.As(err, &le) || le.Status != tc.limit) {
			t.Errorf("%s: want a %d file limit error, got %v", tc.label, tc.limit, err)
		}
	}
}

func TestAnIdempotentFileInsertReplaysOnlyForTheSameBytes(t *testing.T) {
	st, ctx := fileTable(t)
	write := func(sum string) (InsertResult, error) {
		src := &fakeFiles{parts: map[string]FileBlob{"f": blob(sum, 5)}}
		return st.Insert(WithFileSource(ctx, src), "app", "docs", []map[string]any{{"doc": map[string]any{"part": "f"}}}, WriteOpts{IdempotencyKey: "k"}, Embedder{}, nil, Incarnation{})
	}
	first, err := write("a")
	if err != nil {
		t.Fatal(err)
	}
	again, err := write("a")
	if err != nil || again.Ids[0] != first.Ids[0] {
		t.Fatalf("the same bytes should replay: %v %v", again, err)
	}
	if _, err := write("d"); err == nil {
		t.Fatal("different bytes under the same key must not replay")
	}
}

func TestABatchReplaysOnlyForTheSameBytesWhateverTheKeyCase(t *testing.T) {
	st, ctx := fileTable(t)
	write := func(sum string) (BatchResult, error) {
		src := &fakeFiles{parts: map[string]FileBlob{"f": blob(sum, 5)}}
		return st.Batch(WithFileSource(ctx, src), "app", []BatchWrite{{Kind: BatchWriteInsert, Table: "docs", Records: []map[string]any{{"DOC": map[string]any{"part": "f"}}}}}, BatchOpts{IdempotencyKey: "k"}, Embedder{}, nil, Incarnation{})
	}
	if _, err := write("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := write("d"); err == nil {
		t.Fatal("different bytes under the same key must not replay, even when the field is spelled in capitals")
	}
}

func TestTheBatchFingerprintOfATableWithoutFilesIsUnchanged(t *testing.T) {
	st, ctx := fileTable(t)
	if _, err := st.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "title", Type: schema.String}}, TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	writes := []BatchWrite{
		{Kind: BatchWriteUpdate, Table: "notes", Filter: "id = ?", Args: []any{1}, Set: map[string]any{"TITLE": "x"}},
		{Kind: BatchWriteDelete, Table: "notes", Filter: "id = ?", Args: []any{2}},
	}
	n, err := st.ns("app")
	if err != nil {
		t.Fatal(err)
	}
	defer n.unpin()
	sc, err := loadSchema(ctx, n.ro, "app", "notes")
	if err != nil {
		t.Fatal(err)
	}
	want, err := BatchPayloadHash(st.secrets, map[string]*schema.TableSchema{"notes": sc}, writes, BatchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.batchPayloadHash(ctx, n.ro, "app", writes, BatchOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Primary != want.Primary {
		t.Fatal("a stored batch fingerprint must keep matching: the hash of a batch without file fields changed")
	}
}
