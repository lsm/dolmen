package lakehouse

import (
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

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
