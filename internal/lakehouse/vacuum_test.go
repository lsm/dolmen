package lakehouse

import (
	"reflect"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestVacuumCompactsDataAndDeleteFilesIntoOne(t *testing.T) {
	cfg := sidecarConfig(t)
	s, err := Open(t.TempDir(), WithSQLEngine(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	none := store.Incarnation{}
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "ns", "t", []schema.Field{{Name: "v", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"a", "b", "c"} {
		if _, err := s.Insert(ctx, "ns", "t", []map[string]any{{"v": v}}, store.WriteOpts{}, store.Embedder{}, nil, none); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Update(ctx, "ns", "t", "v = ?", []any{"a"}, map[string]any{"v": "a2"}, store.Embedder{}, nil, none); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete(ctx, "ns", "t", "v = ?", []any{"b"}, store.DeleteOpts{}, nil, none); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetRows(ctx, "ns", "t", []int64{1, 2, 3}, nil, none)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Vacuum(ctx, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if res.BytesAfter >= res.BytesBefore {
		t.Fatalf("vacuum must return space: %+v", res)
	}
	after, err := s.GetRows(ctx, "ns", "t", []int64{1, 2, 3}, nil, none)
	if err != nil || !reflect.DeepEqual(before.Rows, after.Rows) {
		t.Fatalf("vacuum must not change what reads return: %+v / %+v %v", before.Rows, after.Rows, err)
	}
	if err := s.withNamespace(ctx, "ns", func(n *namespace) error {
		state, err := loadTable(ctx, n, "ns", "t")
		if err != nil {
			return err
		}
		tasks, err := state.native.Scan().PlanFiles(ctx)
		if err != nil {
			return err
		}
		if len(tasks) != 1 || len(tasks[0].DeleteFiles) != 0 || len(state.native.Metadata().Snapshots()) != 1 {
			t.Errorf("after vacuum: %d data files, %d snapshots", len(tasks), len(state.native.Metadata().Snapshots()))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	q, err := s.Query(ctx, "ns", "SELECT v FROM t ORDER BY id", nil, [16]byte{}, store.Page{})
	if err != nil || len(q.Rows) != 2 || q.Rows[0]["v"] != "a2" {
		t.Fatalf("the SQL sidecar must read the compacted table: %+v %v", q, err)
	}
	if _, err := s.Insert(ctx, "ns", "t", []map[string]any{{"v": "d"}}, store.WriteOpts{}, store.Embedder{}, nil, none); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete(ctx, "ns", "t", "v = ?", []any{"c"}, store.DeleteOpts{}, nil, none); err != nil {
		t.Fatal(err)
	}
	if rows, _ := scannedRows(t, s, "ns", "t"); rows != 2 {
		t.Fatalf("writes after vacuum: %d rows, want 2", rows)
	}
}
