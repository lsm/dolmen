package lakehouse

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type testFiles struct {
	attached, pinned, fetched int
}

func (f *testFiles) Describe(part string) (store.FileBlob, error) {
	if part != "f" {
		return store.FileBlob{}, errors.New("missing part")
	}
	return store.FileBlob{SHA256: strings.Repeat("a", 64), Size: 5, MediaType: "text/plain", Name: "a.txt"}, nil
}
func (f *testFiles) Attach(_ context.Context, _, part string) (store.FileBlob, error) {
	f.attached++
	return f.Describe(part)
}
func (f *testFiles) Pin(_, _ string) error                         { f.pinned++; return nil }
func (f *testFiles) Fetch(context.Context, string) (string, error) { f.fetched++; return "f", nil }

func dormantFileStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), WithSharedFilter(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "ns", "docs", []schema.Field{{Name: "title", Type: schema.String}, {Name: "doc", Type: schema.JSON}}, store.TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if err := s.withNamespace(ctx, "ns", func(n *namespace) error {
		state, err := loadTable(ctx, n, "ns", "docs")
		if err != nil {
			return err
		}
		state.schema.Fields[1] = schema.Field{Name: "doc", Type: schema.File, MaxBytes: 100, MediaTypes: []string{"text/*"}}
		return s.publishSchema(ctx, n, state)
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDormantFileWritesAttachOnceAndReplayBeforeFetching(t *testing.T) {
	s := dormantFileStore(t)
	src := &testFiles{}
	ctx, cancel := context.WithTimeout(store.WithFileSource(t.Context(), src), 10*time.Second)
	defer cancel()
	record := []map[string]any{{"title": "a", "doc": map[string]any{"url": "https://files.example/a", "name": "override.txt"}}}
	opts := store.WriteOpts{IdempotencyKey: "k", Owner: "alice"}
	first, err := s.Insert(ctx, "ns", "docs", record, opts, store.Embedder{}, nil, store.Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.Insert(ctx, "ns", "docs", record, opts, store.Embedder{}, nil, store.Incarnation{})
	if err != nil || !replay.Replayed || len(replay.Ids) != 1 || replay.Ids[0] != first.Ids[0] {
		t.Fatalf("replay %+v: %v", replay, err)
	}
	if src.attached != 1 || src.fetched != 1 {
		t.Fatalf("source touched on replay: %+v", src)
	}
	for _, tc := range []struct{ kind, filter string }{{"update", "id = 1"}, {"upsert", "id = 1"}, {"upsert", "id = 99"}, {"key", ""}} {
		before := src.attached
		set := map[string]any{"title": "a", "doc": map[string]any{"part": "f"}}
		switch tc.kind {
		case "update":
			_, err = s.Update(ctx, "ns", "docs", tc.filter, nil, set, store.Embedder{}, nil, store.Incarnation{})
		case "upsert":
			_, err = s.Upsert(ctx, "ns", "docs", tc.filter, nil, set, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{})
		case "key":
			_, err = s.UpsertByKey(ctx, "ns", "docs", []string{"title"}, []map[string]any{{"title": "key", "doc": set["doc"]}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{})
		}
		if err != nil || src.attached != before+1 {
			t.Fatalf("%s attaches once: %+v, %v", tc.kind, src, err)
		}
	}
	before := src.attached
	if _, err := s.Update(ctx, "ns", "docs", "id = 999", nil, map[string]any{"doc": map[string]any{"part": "f"}}, store.Embedder{}, nil, store.Incarnation{}); err != nil || src.attached != before {
		t.Fatalf("no-match update: %+v %v", src, err)
	}
	if _, err := s.Update(ctx, "ns", "docs", "id = 999", nil, map[string]any{"doc": "raw hash"}, store.Embedder{}, nil, store.Incarnation{}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("invalid file must fail precheck: %v", err)
	}
	if _, err := s.UpsertByKey(ctx, "ns", "docs", []string{"doc"}, []map[string]any{{"doc": map[string]any{"part": "f"}}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); !errors.Is(err, store.ErrInvalid) || src.attached != before {
		t.Fatalf("file key: %+v %v", src, err)
	}
}

func TestDormantCopyReadsAndAuthorizesInsideTheNamespaceGate(t *testing.T) {
	s := dormantFileStore(t)
	src := &testFiles{}
	ctx, cancel := context.WithTimeout(store.WithFileSource(t.Context(), src), 10*time.Second)
	defer cancel()
	if _, err := s.Insert(ctx, "ns", "docs", []map[string]any{{"doc": map[string]any{"part": "f"}}}, store.WriteOpts{Owner: "alice"}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	copyValue := []map[string]any{{"doc": map[string]any{"copy_from": map[string]any{"table": "docs", "id": 1, "field": "doc"}}}}
	for _, owner := range []string{"alice", "bob"} {
		calls := 0
		reach := store.WithCopyReach(ctx, func(ctx context.Context, ns, name string) (*store.RowScope, error) {
			calls++
			if _, _, err := s.TableState(ctx, ns, name, nil); err != nil {
				return nil, err
			}
			return &store.RowScope{Owner: owner}, nil
		})
		res, err := s.Insert(reach, "ns", "docs", copyValue, store.WriteOpts{Owner: owner}, store.Embedder{}, nil, store.Incarnation{})
		if calls != 1 {
			t.Fatalf("reach called %d times", calls)
		}
		if owner == "bob" {
			if !errors.Is(err, store.ErrInvalid) || !strings.Contains(err.Error(), "no file you can read") {
				t.Fatal(err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.GetRows(ctx, "ns", "docs", res.Ids, nil, store.Incarnation{})
		if err != nil || got.Rows[0]["doc"].(map[string]any)["sha256"] != strings.Repeat("a", 64) {
			t.Fatalf("copy %+v %v", got, err)
		}
	}
	if src.pinned != 1 || src.attached != 1 {
		t.Fatalf("copy touches %+v", src)
	}
}

func TestDormantFileBatchReplayAndValidation(t *testing.T) {
	s := dormantFileStore(t)
	src := &testFiles{}
	ctx := store.WithFileSource(t.Context(), src)
	writes := []store.BatchWrite{{Kind: store.BatchWriteInsert, Table: "docs", Records: []map[string]any{{"doc": map[string]any{"part": "f"}}}}, {Kind: store.BatchWriteUpdate, Table: "docs", Filter: "id = 1", Set: map[string]any{"doc": map[string]any{"part": "f", "name": "batch.txt"}}}}
	opts := store.BatchOpts{IdempotencyKey: "batch"}
	if _, err := s.Batch(ctx, "ns", writes, opts, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if src.attached != 2 {
		t.Fatalf("batch attached %d times", src.attached)
	}
	replay, err := s.Batch(ctx, "ns", writes, opts, store.Embedder{}, nil, store.Incarnation{})
	if err != nil || !replay.Replayed || src.attached != 2 {
		t.Fatalf("batch replay %+v, %+v, %v", replay, src, err)
	}
	for _, v := range []any{map[string]any{"part": "f", "media_type": "image/png"}, map[string]any{"sha256": strings.Repeat("a", 64)}, map[string]any{"url": "https://x", "part": "f"}} {
		if _, err := s.Insert(ctx, "ns", "docs", []map[string]any{{"doc": v}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("bad file %v: %v", v, err)
		}
	}
	if src.attached != 2 {
		t.Fatalf("invalid input attached files: %+v", src)
	}
}
