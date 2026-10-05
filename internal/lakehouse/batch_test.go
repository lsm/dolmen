package lakehouse

import (
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestABatchCutShortByACrashIsRolledBackOnReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "ns", "t", []schema.Field{{Name: "x", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "ns", "t", []map[string]any{{"x": "kept"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if err := s.withNamespace(ctx, "ns", func(n *namespace) error { return s.beginBatch(ctx, n, "ns") }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "ns", "t", []map[string]any{{"x": "torn"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	res, err := s.GetRows(ctx, "ns", "t", []int64{1, 2}, nil, store.Incarnation{})
	if err != nil || len(res.Rows) != 1 || res.Rows[0]["x"] != "kept" {
		t.Fatalf("an uncommitted batch must be rolled back on reopen: %+v %v", res, err)
	}
	if _, n, err := s.DescribeTable(ctx, "ns", "t", nil, store.Incarnation{}); err != nil || n != 1 {
		t.Fatalf("row_count after recovery = %d %v", n, err)
	}
	if rows, _ := scannedRows(t, s, "ns", "t"); rows != 1 {
		t.Fatalf("the Iceberg table must be rolled back too: %d rows", rows)
	}
}
