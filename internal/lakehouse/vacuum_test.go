package lakehouse

import (
	"errors"
	"reflect"
	"testing"
	"time"

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

func TestStagedEmbeddingsGoWithTheirTableAndWithAnAbandonedMigration(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kept", "dropped"} {
		if _, err := s.CreateTable(ctx, "ns", name, []schema.Field{{Name: "body", Type: schema.Text}}, store.TableOpts{}, [16]byte{}); err != nil {
			t.Fatal(err)
		}
	}
	staged := func() int {
		t.Helper()
		var n int
		if err := s.namespaces["ns"].db.QueryRowContext(ctx, `SELECT count(*) FROM _dolmen_lakehouse_embed_stage`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	err = s.withNamespace(ctx, "ns", func(n *namespace) error {
		for _, name := range []string{"kept", "dropped"} {
			if err := stageVectors(ctx, n, name, 0, "fake", []int64{1}, []string{"x"}, [][]float32{{1, 0}}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil || staged() != 2 {
		t.Fatalf("staging: %v, %d rows", err, staged())
	}
	if err := s.DropTable(ctx, "ns", "dropped", store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if n := staged(); n != 1 {
		t.Fatalf("drop_table must purge its staged embeddings: %d rows left", n)
	}
	if _, err := s.Vacuum(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	if n := staged(); n != 0 {
		t.Fatalf("vacuum must purge embeddings staged by a migration no longer running: %d rows left", n)
	}
}

func TestVacuumWaitsForARunningQuery(t *testing.T) {
	cfg := sidecarConfig(t)
	s, err := Open(t.TempDir(), WithSQLEngine(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "ns", "t", []schema.Field{{Name: "v", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"a", "b", "c"} {
		if _, err := s.Insert(ctx, "ns", "t", []map[string]any{{"v": v}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Delete(ctx, "ns", "t", "v = 'b'", nil, store.DeleteOpts{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Query(ctx, "ns", "SELECT count(*) AS n FROM t", nil, [16]byte{}, store.Page{}); err != nil {
		t.Fatal(err)
	}
	sc := s.namespaces["ns"].sql
	sc.run.Lock()
	counted := make(chan error, 1)
	go func() {
		res, err := s.Query(ctx, "ns", "SELECT count(*) AS n FROM t", nil, [16]byte{}, store.Page{})
		if err == nil && res.Rows[0]["n"] != int64(2) {
			err = errors.New("wrong count")
		}
		counted <- err
	}()
	time.Sleep(200 * time.Millisecond)
	vacuumed := make(chan error, 1)
	go func() {
		_, err := s.Vacuum(ctx, "ns")
		vacuumed <- err
	}()
	select {
	case err := <-vacuumed:
		sc.run.Unlock()
		t.Fatalf("vacuum must not rewrite or delete files while a query is running: it finished first (%v)", err)
	case <-time.After(500 * time.Millisecond):
	}
	sc.run.Unlock()
	if err := <-counted; err != nil {
		t.Fatalf("a query running across a vacuum must still read its files: %v", err)
	}
	if err := <-vacuumed; err != nil {
		t.Fatal(err)
	}
}
