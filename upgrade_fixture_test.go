package dolmen

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func openReleaseFixture(t *testing.T, release string) *Store {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", release, "fixture.db"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fixture.db"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("a namespace written by %s must open: %v", release, err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestANamespaceWrittenByV030IsReadAndWrittenByThisRelease(t *testing.T) {
	st := openReleaseFixture(t, "v0.3.0")
	ctx := context.Background()

	sc, _, err := st.DescribeTable(ctx, "fixture", "notes")
	if err != nil {
		t.Fatal(err)
	}
	if sc.Version != 2 || len(sc.Fields) != 7 {
		t.Fatalf("the migrated v0.3.0 schema must read back whole: version %d, %d fields", sc.Version, len(sc.Fields))
	}

	rows, err := st.Query(ctx, "fixture", `SELECT n, done, kind, tag FROM notes ORDER BY n`, QueryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Rows) != 2 || rows.Rows[1]["done"] != true || rows.Rows[1]["tag"] != "winter" || rows.Rows[0]["done"] != false {
		t.Fatalf("v0.3.0 rows, updates and deletes must read back as written: %v", rows.Rows)
	}

	fts, err := st.SearchFulltext(ctx, "fixture", "notes", "overheat*", SearchOptions{})
	if err != nil || len(fts.Rows) != 1 || fts.Rows[0]["title"] != "overheating pump" {
		t.Fatalf("the v0.3.0 full-text index must answer: %v %v", fts.Rows, err)
	}

	vec, err := st.SearchVector(ctx, "fixture", "notes", VectorQuery{Vec: []float32{0, 1, 0}, Column: "emb"}, SearchOptions{Limit: 1})
	if err != nil || len(vec.Rows) != 1 || vec.Rows[0]["title"] != "cold start" || vec.SkippedVectors != 0 {
		t.Fatalf("v0.3.0 vectors must decode and rank: %v skipped %d %v", vec.Rows, vec.SkippedVectors, err)
	}

	if _, err := st.Insert(ctx, "fixture", "notes", []map[string]any{{"title": "new pump", "body": "overheats again", "emb": []float32{1, 0, 0}}}, InsertOptions{}); err != nil {
		t.Fatalf("this release must write into a v0.3.0 namespace: %v", err)
	}
	fts, err = st.SearchFulltext(ctx, "fixture", "notes", "overheat*", SearchOptions{})
	if err != nil || len(fts.Rows) != 2 {
		t.Fatalf("a row written now must join the v0.3.0 full-text index: %v %v", fts.Rows, err)
	}
	if _, err := st.Update(ctx, "fixture", "notes", UpdateOptions{Filter: "n = ?", Args: []any{1}, Set: map[string]any{"kind": "b"}}); err != nil {
		t.Fatalf("an update against the v0.3.0 enum must pass: %v", err)
	}
}
