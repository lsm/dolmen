package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/schema"
)

type fakeFiles struct {
	parts    map[string]FileBlob
	attached []string
	pinned   []string
	urls     map[string]string
	fetched  []string
}

func (f *fakeFiles) Describe(part string) (FileBlob, error) {
	b, ok := f.parts[part]
	if !ok {
		return FileBlob{}, invalidf("no part %q", part)
	}
	return b, nil
}

func (f *fakeFiles) Fetch(_ context.Context, url string) (string, error) {
	part, ok := f.urls[url]
	if !ok {
		return "", invalidf("cannot fetch %s", url)
	}
	f.fetched = append(f.fetched, url)
	return part, nil
}

func (f *fakeFiles) Pin(ns, sum string) error {
	f.pinned = append(f.pinned, ns+":"+sum)
	return nil
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
		{"unfetchable url", WithFileSource(ctx, src), map[string]any{"url": "https://x"}, "cannot fetch", 0},
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
	want, err := BatchPayloadHash(ctx, st.secrets, map[string]*schema.TableSchema{"notes": sc}, writes, BatchOpts{})
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

type recordingSweeper struct {
	refs   map[string]bool
	during func()
	called int
}

func (r *recordingSweeper) Sweep(_ context.Context, ns string, refs map[string]bool) (FileSweep, error) {
	r.called++
	r.refs = refs
	if r.during != nil {
		r.during()
	}
	return FileSweep{Removed: 1, RemovedBytes: 5, Bytes: 10}, nil
}

func TestVacuumSweepsWithEveryLiveReferenceWhileHoldingTheWriter(t *testing.T) {
	st, ctx := fileTable(t)
	src := &fakeFiles{parts: map[string]FileBlob{"a": blob("a", 5), "b": blob("b", 5)}}
	if _, err := st.Insert(WithFileSource(ctx, src), "app", "docs", []map[string]any{
		{"title": "keep", "doc": map[string]any{"part": "a"}},
		{"title": "drop", "doc": map[string]any{"part": "b"}},
	}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Update(ctx, "app", "docs", "title = ?", []any{"drop"}, map[string]any{"doc": nil}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	var blocked error
	sw := &recordingSweeper{during: func() {
		wctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		_, blocked = st.Insert(wctx, "app", "docs", []map[string]any{{"title": "racing"}}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	}}
	res, err := st.Vacuum(WithFileSweeper(ctx, sw), "app")
	if err != nil {
		t.Fatal(err)
	}
	if sw.called != 1 || len(sw.refs) != 1 || !sw.refs[strings.Repeat("a", 64)] {
		t.Fatalf("the sweep must see exactly the live references, got %v", sw.refs)
	}
	if blocked == nil {
		t.Fatal("a write must wait while the sweep holds the namespace's writer")
	}
	if res.Files == nil || res.Files.Removed != 1 || res.Files.Bytes != 10 {
		t.Fatalf("vacuum must report the sweep, got %+v", res.Files)
	}
	plain, err := st.Vacuum(ctx, "app")
	if err != nil || plain.Files != nil {
		t.Fatalf("without a sweeper vacuum reports no files: %+v %v", plain, err)
	}
}

func TestAVacuumThatFailsSweepsNothing(t *testing.T) {
	st, ctx := fileTable(t)
	other, err := sql.Open("sqlite", dsn(st.nsPath("app"), false))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	conn, err := other.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(ctx, `ROLLBACK`)
	sw := &recordingSweeper{}
	vctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := st.Vacuum(WithFileSweeper(vctx, sw), "app"); err == nil {
		t.Fatal("vacuum should fail while another connection holds the write lock")
	}
	if sw.called != 0 {
		t.Fatal("a vacuum that did not run must not delete files")
	}
}

func TestCopyFromReusesAReadableRowsFile(t *testing.T) {
	st, ctx := fileTable(t)
	if _, err := st.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "meta", Type: schema.JSON}}, TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	src := &fakeFiles{parts: map[string]FileBlob{"f": blob("a", 5)}}
	if _, err := st.Insert(WithFileSource(ctx, src), "app", "docs", []map[string]any{{"doc": map[string]any{"part": "f"}}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Insert(ctx, "app", "notes", []map[string]any{{"meta": map[string]any{"sha256": strings.Repeat("b", 64), "size": 1, "media_type": "text/plain"}}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	copyOf := func(table string, id int, field string, extra map[string]any) map[string]any {
		v := map[string]any{"copy_from": map[string]any{"table": table, "id": id, "field": field}}
		for k, x := range extra {
			v[k] = x
		}
		return map[string]any{"doc": v}
	}
	res, err := st.Insert(WithFileSource(ctx, src), "app", "docs", []map[string]any{copyOf("docs", 1, "doc", map[string]any{"name": "copy.txt"})}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetRows(ctx, "app", "docs", res.Ids, nil, Incarnation{})
	ref := got.Rows[0]["doc"].(map[string]any)
	if ref["sha256"] != strings.Repeat("a", 64) || ref["name"] != "copy.txt" || ref["media_type"] != "text/plain" {
		t.Fatalf("copied reference = %v", ref)
	}
	if len(src.pinned) != 1 || src.pinned[0] != "app:"+strings.Repeat("a", 64) {
		t.Fatalf("the copied hash must be pinned until the write ends, got %v", src.pinned)
	}
	denied := WithCopyReach(WithFileSource(ctx, src), func(context.Context, string, string) (*RowScope, error) { return nil, errors.New("forbidden") })
	for label, tc := range map[string]struct {
		ctx context.Context
		rec map[string]any
	}{
		"unreadable table": {denied, copyOf("docs", 1, "doc", nil)},
		"missing row":      {WithFileSource(ctx, src), copyOf("docs", 99, "doc", nil)},
		"json lookalike":   {WithFileSource(ctx, src), copyOf("notes", 1, "meta", nil)},
		"empty file field": {WithFileSource(ctx, src), copyOf("docs", 99, "doc", nil)},
	} {
		_, err := st.Insert(tc.ctx, "app", "docs", []map[string]any{tc.rec}, WriteOpts{}, Embedder{}, nil, Incarnation{})
		if err == nil || !strings.Contains(err.Error(), "copy_from names no file you can read") {
			t.Errorf("%s: %v", label, err)
		}
	}
	_, err = st.Insert(WithFileSource(ctx, src), "app", "docs", []map[string]any{copyOf("docs", 1, "doc", map[string]any{"media_type": "application/pdf"})}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	var le *FileLimitError
	if !errors.As(err, &le) || le.Status != 415 {
		t.Fatalf("a copy must satisfy the target field's media_types: %v", err)
	}
}

func TestCopyFromNormalizesTheSourceTable(t *testing.T) {
	st, ctx := fileTable(t)
	src := &fakeFiles{parts: map[string]FileBlob{"f": blob("a", 5)}}
	if _, err := st.Insert(WithFileSource(ctx, src), "app", "docs", []map[string]any{{"doc": map[string]any{"part": "f"}}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	var reached string
	cctx := WithCopyReach(WithFileSource(ctx, src), func(_ context.Context, _, table string) (*RowScope, error) {
		reached = table
		return nil, nil
	})
	rec := map[string]any{"doc": map[string]any{"copy_from": map[string]any{"table": " DOCS ", "id": 1, "field": "doc"}}}
	if _, err := st.Insert(cctx, "app", "docs", []map[string]any{rec}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("a source table spelled with spaces and capitals must resolve: %v", err)
	}
	if reached != "docs" {
		t.Fatalf("authorization saw table %q, want the normalized name", reached)
	}
}

func TestAURLValueFetchesThroughTheFileSource(t *testing.T) {
	st, ctx := fileTable(t)
	src := &fakeFiles{parts: map[string]FileBlob{"fetched": blob("c", 7)}, urls: map[string]string{"https://files.example/a.txt": "fetched"}}
	rec := func() []map[string]any {
		return []map[string]any{{"doc": map[string]any{"url": "https://files.example/a.txt", "name": "a.txt"}}}
	}
	first, err := st.Insert(WithFileSource(ctx, src), "app", "docs", rec(), WriteOpts{IdempotencyKey: "k"}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetRows(ctx, "app", "docs", first.Ids, nil, Incarnation{})
	if ref := got.Rows[0]["doc"].(map[string]any); ref["sha256"] != strings.Repeat("c", 64) || ref["name"] != "a.txt" {
		t.Fatalf("fetched reference = %v", ref)
	}
	again, err := st.Insert(WithFileSource(ctx, src), "app", "docs", rec(), WriteOpts{IdempotencyKey: "k"}, Embedder{}, nil, Incarnation{})
	if err != nil || !again.Replayed || len(src.fetched) != 1 {
		t.Fatalf("an idempotent retry must replay without fetching again: %+v %v fetched=%v", again, err, src.fetched)
	}
	if _, err := st.Insert(WithFileSource(ctx, src), "app", "docs", []map[string]any{{"doc": map[string]any{"url": "https://x", "part": "fetched"}}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err == nil || !strings.Contains(err.Error(), "exactly one of") {
		t.Fatalf("a value with two sources must be refused: %v", err)
	}
}

func TestSharedFileFingerprintsCanonicalizeWithoutAttaching(t *testing.T) {
	sc := &schema.TableSchema{Fields: []schema.Field{{Name: "doc", Type: schema.File}}}
	src := &fakeFiles{parts: map[string]FileBlob{"first": blob("a", 5), "renamed": blob("a", 5)}}
	ctx := WithFileSource(context.Background(), src)
	record := func(part string) []map[string]any {
		return []map[string]any{{"doc": map[string]any{"part": part, "name": "override.txt"}}}
	}
	canonical := []map[string]any{{"doc": map[string]any{"sha256": strings.Repeat("a", 64), "media_type": "text/plain", "name": "override.txt"}}}
	want, err := RequestHash(context.Background(), nil, sc, canonical)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"first", "renamed"} {
		got, err := RequestHash(ctx, nil, sc, record(part))
		if err != nil || got.Primary != want.Primary {
			t.Fatalf("insert hash = %v, %v; want %v", got, err, want)
		}
		for _, kind := range []BatchWriteKind{BatchWriteInsert, BatchWriteUpdate} {
			w := BatchWrite{Kind: kind, Table: "docs"}
			expected := w
			if kind == BatchWriteInsert {
				w.Records = []map[string]any{{"DOC": record(part)[0]["doc"]}}
				expected.Records = canonical
			} else {
				w.Set = map[string]any{"DOC": record(part)[0]["doc"]}
				expected.Set = canonical[0]
			}
			got, err := BatchPayloadHash(ctx, nil, map[string]*schema.TableSchema{"docs": sc}, []BatchWrite{w}, BatchOpts{})
			want, wantErr := BatchPayloadHash(context.Background(), nil, map[string]*schema.TableSchema{"docs": sc}, []BatchWrite{expected}, BatchOpts{})
			if err != nil || wantErr != nil || got.Primary != want.Primary {
				t.Fatalf("%s hash = %v, %v; want %v, %v", kind, got, err, want, wantErr)
			}
		}
	}
	if len(src.attached)+len(src.pinned)+len(src.fetched) != 0 {
		t.Fatalf("hashing touched files: %+v", src)
	}
	url := []map[string]any{{"doc": map[string]any{"url": "https://example.com/doc", "name": "x.txt"}}}
	a, _ := RequestHash(ctx, nil, sc, url)
	b, _ := RequestHash(context.Background(), nil, sc, url)
	if a.Primary != b.Primary {
		t.Fatal("URL hashing must not resolve the URL")
	}
}

func TestParseFileReferencesFromStoredAndPresentedValues(t *testing.T) {
	want := blob("a", 5)
	raw, err := fileReference(want)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []any{raw, map[string]any{"sha256": want.SHA256, "size": 5, "media_type": want.MediaType, "name": want.Name}} {
		got, err := ParseFileReference(v)
		if err != nil || got != want {
			t.Fatalf("reference %v = %+v, %v", v, got, err)
		}
	}
	for _, v := range []any{nil, "{}", map[string]any{"sha256": "a", "media_type": "text/plain", "size": -1}} {
		if _, err := ParseFileReference(v); !errors.Is(err, ErrCatalogCorrupt) {
			t.Fatalf("reference %v: %v", v, err)
		}
	}
}

func TestAnUpdateThatMatchesNoRowStoresNoFile(t *testing.T) {
	st, ctx := fileTable(t)
	if _, err := st.Insert(ctx, "app", "docs", []map[string]any{{"title": "kept"}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	src := &fakeFiles{parts: map[string]FileBlob{"f": blob("a", 5), "big": blob("b", 200)}}
	fctx := WithFileSource(ctx, src)
	res, err := st.Update(fctx, "app", "docs", "id = 999", nil, map[string]any{"doc": map[string]any{"part": "f"}}, Embedder{}, nil, Incarnation{})
	if err != nil || res.Updated != 0 {
		t.Fatalf("update = %+v %v", res, err)
	}
	if len(src.attached) != 0 {
		t.Fatalf("an update matching no row attached %v, which only a vacuum would remove", src.attached)
	}
	if _, err := st.Update(fctx, "app", "docs", "id = 999", nil, map[string]any{"doc": map[string]any{"part": "big"}}, Embedder{}, nil, Incarnation{}); err == nil || !strings.Contains(err.Error(), "max_bytes") {
		t.Fatalf("a file over max_bytes must still be refused when no row matches: %v", err)
	}
	if res, err := st.Update(fctx, "app", "docs", "id = 1", nil, map[string]any{"doc": map[string]any{"part": "f"}}, Embedder{}, nil, Incarnation{}); err != nil || res.Updated != 1 || len(src.attached) != 1 {
		t.Fatalf("update of a matching row = %+v %v, attached %v", res, err, src.attached)
	}
}
