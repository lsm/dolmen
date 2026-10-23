package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
)

func importFixture(t *testing.T) *Store {
	t.Helper()
	st := openStore(t)
	mustNS(t, st, "test")
	if _, err := st.CreateTable(context.Background(), "test", "events", []schema.Field{
		{Name: "body", Type: schema.Text, Fulltext: true},
		{Name: "active", Type: schema.Boolean},
		{Name: "meta", Type: schema.JSON},
	}); err != nil {
		t.Fatal(err)
	}
	return st.Store
}

func sourceRow(id any, body string) map[string]any {
	return map[string]any{"id": id, "created_at": "2026-01-02T03:04:05.123Z", "body": body, "active": true, "meta": map[string]any{"n": json.Number("7")}}
}

func TestImportPreservesIdentityAndResumes(t *testing.T) {
	st, ctx := importFixture(t), context.Background()
	rows := []map[string]any{sourceRow(json.Number("9007199254740993"), "stone memory"), sourceRow(int64(42), "second")}
	got, err := st.ImportRecords(ctx, "test", "events", rows, WriteOpts{IdempotencyKey: "chunk1"}, Embedder{}, Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Inserted != 2 || got.Skipped != 0 || !reflect.DeepEqual(got.Ids, []int64{9007199254740993, 42}) {
		t.Fatalf("import: %+v", got)
	}
	read, err := st.GetRows(ctx, "test", "events", []int64{42, 9007199254740993}, nil, Incarnation{})
	if err != nil || len(read.Rows) != 2 || read.Rows[0]["created_at"] != "2026-01-02T03:04:05.123Z" {
		t.Fatalf("read: %+v %v", read, err)
	}
	replay, err := st.ImportRecords(ctx, "test", "events", rows, WriteOpts{IdempotencyKey: "chunk1"}, Embedder{}, Incarnation{})
	if err != nil || !replay.Replayed || replay.Inserted != 2 {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	retry, err := st.ImportRecords(ctx, "test", "events", rows[1:], WriteOpts{IdempotencyKey: "regrouped"}, Embedder{}, Incarnation{})
	if err != nil || retry.Skipped != 1 || retry.Inserted != 0 || retry.Changes.Count != 0 {
		t.Fatalf("resume: %+v %v", retry, err)
	}
	next, err := st.Insert(ctx, "test", "events", []map[string]any{{"body": "ordinary"}}, WriteOpts{IdempotencyKey: "chunk1"}, Embedder{}, nil, Incarnation{})
	if err != nil || next.Ids[0] <= 9007199254740993 {
		t.Fatalf("allocator or receipt domain: %+v %v", next, err)
	}
	if _, err = st.Insert(ctx, "test", "events", rows, WriteOpts{}, Embedder{}, nil, Incarnation{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("normal insert accepted metadata: %v", err)
	}
}

func TestImportConflictRollsBackChunk(t *testing.T) {
	st, ctx := importFixture(t), context.Background()
	if _, err := st.ImportRecords(ctx, "test", "events", []map[string]any{sourceRow(42, "original")}, WriteOpts{IdempotencyKey: "one"}, Embedder{}, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	_, err := st.ImportRecords(ctx, "test", "events", []map[string]any{sourceRow(43, "new"), sourceRow(42, "different")}, WriteOpts{IdempotencyKey: "two"}, Embedder{}, Incarnation{})
	if !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("conflict: %v", err)
	}
	read, err := st.GetRows(ctx, "test", "events", []int64{42, 43}, nil, Incarnation{})
	if err != nil || len(read.Rows) != 1 || read.Rows[0]["body"] != "original" {
		t.Fatalf("partial write: %+v %v", read, err)
	}
	_, err = st.ImportRecords(ctx, "test", "events", []map[string]any{sourceRow(44, "changed")}, WriteOpts{IdempotencyKey: "one"}, Embedder{}, Incarnation{})
	if !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("key reuse: %v", err)
	}
}

func TestImportRejectsInvalidMetadataAtomically(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"missing id":      func(r map[string]any) { delete(r, "id") },
		"zero":            func(r map[string]any) { r["id"] = 0 },
		"fraction":        func(r map[string]any) { r["id"] = 1.5 },
		"overflow":        func(r map[string]any) { r["id"] = json.Number("9223372036854775808") },
		"max":             func(r map[string]any) { r["id"] = int64(math.MaxInt64) },
		"imprecise float": func(r map[string]any) { r["id"] = float64(9007199254740994) },
		"microseconds":    func(r map[string]any) { r["created_at"] = "2026-01-02T03:04:05.123456Z" },
		"offset":          func(r map[string]any) { r["created_at"] = "2026-01-02T04:04:05.123+01:00" },
		"unknown":         func(r map[string]any) { r["unknown"] = true },
	} {
		t.Run(name, func(t *testing.T) {
			st := importFixture(t)
			row := sourceRow(42, "bad")
			mutate(row)
			_, err := st.ImportRecords(context.Background(), "test", "events", []map[string]any{sourceRow(41, "valid"), row}, WriteOpts{IdempotencyKey: "bad"}, Embedder{}, Incarnation{})
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid input: %v", err)
			}
			read, err := st.GetRows(context.Background(), "test", "events", []int64{41, 42}, nil, Incarnation{})
			if err != nil || len(read.Rows) != 0 {
				t.Fatalf("partial validation: %+v %v", read, err)
			}
		})
	}
}

func TestImportSearchReopenAndConcurrentNormalWrites(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { st.Close() }()
	if err = st.CreateNamespace(ctx, "test", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.CreateTable(ctx, "test", "events", []schema.Field{{Name: "body", Type: schema.Text, Fulltext: true}}, TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	const key = "snapshot"
	row := map[string]any{"id": int64(10000), "created_at": "2026-01-02T03:04:05.123Z", "body": "granite archive"}
	if _, err = st.ImportRecords(ctx, "test", "events", []map[string]any{row}, WriteOpts{IdempotencyKey: key}, Embedder{}, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	found, err := st.SearchFulltext(ctx, "test", "events", "granite", "", nil, false, nil, Incarnation{}, Page{Limit: 10})
	if err != nil || len(found.Rows) != 1 || found.Rows[0]["id"] != int64(10000) {
		t.Fatalf("FTS: %+v %v", found, err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := st.ImportRecords(ctx, "test", "events", []map[string]any{row}, WriteOpts{IdempotencyKey: "new-key"}, Embedder{}, Incarnation{})
	if err != nil || resumed.Skipped != 1 {
		t.Fatalf("reopen: %+v %v", resumed, err)
	}
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func(i int) {
			if i%2 == 0 {
				_, e := st.ImportRecords(ctx, "test", "events", []map[string]any{{"id": int64(20000), "created_at": row["created_at"], "body": "new imported"}}, WriteOpts{IdempotencyKey: fmt.Sprintf("parallel-%d", i)}, Embedder{}, Incarnation{})
				errs <- e
			} else {
				_, e := st.Insert(ctx, "test", "events", []map[string]any{{"body": "new ordinary"}}, WriteOpts{}, Embedder{}, nil, Incarnation{})
				errs <- e
			}
		}(i)
	}
	for range 8 {
		if err = <-errs; err != nil {
			t.Fatal(err)
		}
	}
	all, err := st.ListRows(ctx, "test", "events", 0, 100, nil, Incarnation{})
	if err != nil || len(all.Rows) != 6 {
		t.Fatalf("parallel rows: %+v %v", all, err)
	}
	t.Logf("real SQLite: id=%v stamp=%v FTS=1 reopen.skipped=%d concurrent rows=%d", all.Rows[0]["id"], all.Rows[0]["created_at"], resumed.Skipped, len(all.Rows))
}

func TestImportBoundariesAndDefaults(t *testing.T) {
	st, ctx := importFixture(t), context.Background()
	for name, records := range map[string][]map[string]any{"empty": nil, "duplicate": {sourceRow(42, "one"), sourceRow(42, "one")}, "limit": make([]map[string]any, 1001)} {
		t.Run(name, func(t *testing.T) {
			if _, err := st.ImportRecords(ctx, "test", "events", records, WriteOpts{IdempotencyKey: name}, Embedder{}, Incarnation{}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("bounds: %v", err)
			}
		})
	}
	for _, key := range []string{"", "\x00", "é", strings.Repeat("a", 257)} {
		if _, err := st.ImportRecords(ctx, "test", "events", []map[string]any{sourceRow(42, "one")}, WriteOpts{IdempotencyKey: key}, Embedder{}, Incarnation{}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("key: %v", err)
		}
	}
	if _, err := st.CreateTable(ctx, "test", "defaults", []schema.Field{{Name: "stamp", Type: schema.Timestamp, Default: schema.NowDefault}, {Name: "flag", Type: schema.Boolean, Default: true}}, TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	row := map[string]any{"id": 42, "created_at": "2026-01-02T03:04:05.123Z"}
	for _, key := range []string{"first", "second"} {
		got, err := st.ImportRecords(ctx, "test", "defaults", []map[string]any{row}, WriteOpts{IdempotencyKey: key}, Embedder{}, Incarnation{})
		if err != nil || (key == "second" && got.Skipped != 1) {
			t.Fatalf("defaults: %+v %v", got, err)
		}
	}
}

func TestImportSecretsVectorsAndFilesSkipByContent(t *testing.T) {
	ctx := context.Background()
	legacy := openSecretStore(t, t.TempDir(), testKeyring(t, 7))
	mustNS(t, legacy, "test")
	st := legacy.Store
	if _, err := st.CreateTable(ctx, "test", "vault", secretFields(), TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	row := map[string]any{"id": 42, "created_at": "2026-01-02T03:04:05.123Z", "name": "synthetic", "token": "synthetic-value", "emb": []any{1, 0, 0}}
	for _, key := range []string{"one", "two"} {
		got, err := st.ImportRecords(ctx, "test", "vault", []map[string]any{row}, WriteOpts{IdempotencyKey: key}, Embedder{}, Incarnation{})
		if err != nil || (key == "two" && got.Skipped != 1) {
			t.Fatalf("secret/vector: %+v %v", got, err)
		}
	}
	row["token"] = "different-synthetic-value"
	if _, err := st.ImportRecords(ctx, "test", "vault", []map[string]any{row}, WriteOpts{IdempotencyKey: "three"}, Embedder{}, Incarnation{}); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("secret conflict: %v", err)
	}
	files, fctx := fileTable(t)
	src := &fakeFiles{parts: map[string]FileBlob{"f": blob("a", 5)}}
	fctx = WithFileSource(fctx, src)
	fileRow := map[string]any{"id": 42, "created_at": row["created_at"], "doc": map[string]any{"part": "f"}}
	for _, key := range []string{"one", "two"} {
		got, err := files.ImportRecords(fctx, "app", "docs", []map[string]any{fileRow}, WriteOpts{IdempotencyKey: key}, Embedder{}, Incarnation{})
		if err != nil || (key == "two" && got.Skipped != 1) {
			t.Fatalf("file: %+v %v", got, err)
		}
	}
}

func TestImportHashesNormalizedSecretFields(t *testing.T) {
	ctx := context.Background()
	legacy := openSecretStore(t, t.TempDir(), testKeyring(t, 8))
	mustNS(t, legacy, "test")
	st := legacy.Store
	if _, err := st.CreateTable(ctx, "test", "vault", secretFields(), TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	row := map[string]any{"ID": 42, "CREATED_AT": "2026-01-02T03:04:05.123Z", "TOKEN": "synthetic-case-value"}
	if _, err := st.ImportRecords(ctx, "test", "vault", []map[string]any{row}, WriteOpts{IdempotencyKey: "canonical"}, Embedder{}, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	canonical := map[string]any{"id": int64(42), "created_at": row["CREATED_AT"], "token": row["TOKEN"]}
	result, err := st.ImportRecords(ctx, "test", "vault", []map[string]any{canonical}, WriteOpts{IdempotencyKey: "canonical"}, Embedder{}, Incarnation{})
	if err != nil || !result.Replayed {
		t.Fatalf("normalized receipt: %+v %v", result, err)
	}
}

func TestImportOversizedExistingRowHasImportRemediation(t *testing.T) {
	for _, kind := range []string{"accumulated", "escaped", "secret"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			legacy := openSecretStore(t, t.TempDir(), testKeyring(t, 9))
			mustNS(t, legacy, "test")
			st := legacy.Store
			fields := []schema.Field{{Name: "payload_a", Type: schema.Text}, {Name: "payload_b", Type: schema.Text}}
			rec := map[string]any{"payload_a": strings.Repeat("a", MaxQueryBytes/2+1), "payload_b": strings.Repeat("b", MaxQueryBytes/2+1)}
			if kind == "escaped" {
				rec["payload_a"] = strings.Repeat("\n", MaxQueryBytes/2)
				rec["payload_b"] = "x"
			}
			if kind == "secret" {
				fields = []schema.Field{{Name: "payload_a", Type: schema.Secret}}
				rec = map[string]any{"payload_a": strings.Repeat("s", MaxQueryBytes+1)}
			}
			if _, err := st.CreateTable(ctx, "test", "large", fields, TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			inserted, err := st.Insert(ctx, "test", "large", []map[string]any{rec}, WriteOpts{}, Embedder{}, nil, Incarnation{})
			if err != nil {
				t.Fatal(err)
			}
			_, readErr := st.GetRows(ctx, "test", "large", []int64{inserted.Ids[0]}, nil, Incarnation{})
			wantRead := "invalid request: search result exceeds the 32 MiB response budget on its first row"
			if kind == "secret" {
				wantRead = "invalid request: column \"payload_a\" exceeds the 32 MiB response budget; select fewer or smaller columns"
			}
			if !errors.Is(readErr, ErrInvalid) || readErr.Error() != wantRead {
				t.Fatalf("read classification/message: %v", readErr)
			}
			rec["id"] = inserted.Ids[0]
			rec["created_at"] = "2026-01-02T03:04:05.123Z"
			_, err = st.ImportRecords(ctx, "test", "large", []map[string]any{{"id": int64(42), "created_at": rec["created_at"]}, rec}, WriteOpts{IdempotencyKey: "oversized"}, Embedder{}, Incarnation{})
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "existing import row exceeds the read budget") || strings.Contains(err.Error(), "select fewer") {
				t.Fatalf("import remediation: %v", err)
			}
			rows, err := st.GetRows(ctx, "test", "large", []int64{42}, nil, Incarnation{})
			if err != nil || len(rows.Rows) != 0 {
				t.Fatalf("oversized comparison partially wrote: %+v %v", rows, err)
			}
		})
	}
}

func TestImportRetainsFieldRemediation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		field  schema.Field
		values map[string]any
	}{
		{"missing file", schema.Field{Name: "payload", Type: schema.File}, map[string]any{"payload": map[string]any{"part": "upload"}}},
		{"NUL", schema.Field{Name: "payload", Type: schema.Text}, map[string]any{"payload": "a\x00b"}},
		{"enum", schema.Field{Name: "payload", Type: schema.String, Enum: []string{"stone", "wood"}}, map[string]any{"payload": "metal"}},
		{"vector", schema.Field{Name: "payload", Type: schema.Vector, Dim: 3}, map[string]any{"payload": []any{1, 2}}},
		{"unknown", schema.Field{Name: "payload", Type: schema.Text}, map[string]any{"bogus": true}},
		{"required", schema.Field{Name: "payload", Type: schema.Text, Required: true}, map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, ctx := importFixture(t), context.Background()
			if _, err := st.CreateTable(ctx, "test", "invalid_rows", []schema.Field{tc.field}, TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			_, ordinary := st.Insert(ctx, "test", "invalid_rows", []map[string]any{tc.values}, WriteOpts{}, Embedder{}, nil, Incarnation{})
			if !errors.Is(ordinary, ErrInvalid) {
				t.Fatalf("fixture: %v", ordinary)
			}
			row := map[string]any{"id": 42, "created_at": "2026-01-02T03:04:05.123Z"}
			for k, v := range tc.values {
				row[k] = v
			}
			_, err := st.ImportRecords(ctx, "test", "invalid_rows", []map[string]any{row}, WriteOpts{IdempotencyKey: "bad-field"}, Embedder{}, Incarnation{})
			want := strings.TrimPrefix(ordinary.Error(), "invalid request: ")
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "import record 0") || !strings.Contains(err.Error(), want) {
				t.Fatalf("lost ordinary remediation %q: %v", want, err)
			}
		})
	}
}

type importRejectedFileSource struct {
	*fakeFiles
	cause error
}

func (s *importRejectedFileSource) Describe(string) (FileBlob, error) { return FileBlob{}, s.cause }

func TestImportPreservesCoercionErrorChain(t *testing.T) {
	st, ctx := fileTable(t)
	cause := invalidf("synthetic file unavailable; supply the upload in this request")
	ctx = WithFileSource(ctx, &importRejectedFileSource{fakeFiles: &fakeFiles{}, cause: cause})
	_, err := st.ImportRecords(ctx, "app", "docs", []map[string]any{{"id": 42, "created_at": "2026-01-02T03:04:05.123Z", "doc": map[string]any{"part": "upload"}}}, WriteOpts{IdempotencyKey: "bad-file"}, Embedder{}, Incarnation{})
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "import record 0") {
		t.Fatalf("coercion chain lost: %v", err)
	}
}
