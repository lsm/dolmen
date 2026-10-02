package fixture

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheFixtureWritesParquetIcebergAndPositionDeletes(t *testing.T) {
	dir := t.TempDir()
	tbl, err := Write(context.Background(), dir, 50)
	if err != nil {
		t.Fatal(err)
	}

	var parquet, deletes, metadata int
	err = filepath.Walk(filepath.Join(dir, NSName), func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		switch {
		case strings.HasSuffix(p, ".parquet") && strings.Contains(p, "deletes"):
			deletes++
		case strings.HasSuffix(p, ".parquet"):
			parquet++
		case strings.HasSuffix(p, ".metadata.json"):
			metadata++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if parquet < 1 {
		t.Fatalf("no Parquet data files were written under %s", filepath.Join(dir, NSName))
	}
	if deletes < 1 {
		t.Fatal("no position delete file was written, so the delete path the sidecars must honour is not exercised")
	}
	if metadata < 1 {
		t.Fatal("no Iceberg metadata file was written")
	}
	ids := SnapshotIDs(tbl)
	if len(ids) < 3 {
		t.Fatalf("want at least 3 snapshots (append, append, delete), got %d: %v", len(ids), ids)
	}
	if cur := CurrentSnapshotID(tbl); cur != ids[len(ids)-1] {
		t.Fatalf("current snapshot %d is not the newest of %v", cur, ids)
	}
	if got, want := tbl.RowCount, 51; got != want {
		t.Fatalf("row count %d, want %d", got, want)
	}
}

func TestTheCatalogLandsInTheNamespacesOwnSQLiteFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := Write(context.Background(), dir, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "catalog.db")); err != nil {
		t.Fatalf("the catalog is not in the namespace's own SQLite file: %v", err)
	}
}

func TestTwoNamespacesGetSeparateDirectories(t *testing.T) {
	dir := t.TempDir()
	if _, err := Write(context.Background(), dir, 5); err != nil {
		t.Fatal(err)
	}
	first := TableDataDir(dir)
	second := filepath.Join(dir, "other", TableName)
	if first == second {
		t.Fatal("two namespaces share a directory, so a confinement test cannot tell them apart")
	}
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "other.parquet"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(first, "other.parquet")); err == nil {
		t.Fatal("the second namespace's file is visible under the first")
	}
}

func TestThePositionDeleteTargetsTheDataFileThatHoldsTheDeletedRow(t *testing.T) {
	dir := t.TempDir()
	tbl, err := Write(context.Background(), dir, 50)
	if err != nil {
		t.Fatal(err)
	}
	deleted := DeletedPositions()
	path, err := dataFileHolding(tbl.DataDir, deleted[0])
	if err != nil {
		t.Fatal(err)
	}
	ids, err := idsInParquet(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range deleted {
		found := false
		for _, id := range ids {
			if id == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("the delete file targets %s, which does not hold id %d; it holds %v", path, want, ids)
		}
	}
}

func TestTheLiveRowCountIsDerivedRatherThanGuessed(t *testing.T) {
	const seeded = 50
	if got, want := LiveRowsBeforeDelete(seeded), 33; got != want {
		t.Fatalf("live rows before the delete is %d, want %d", got, want)
	}
	if got, want := DeletedLiveRows(), 2; got != want {
		t.Fatalf("deleted rows that were live is %d, want %d", got, want)
	}
	if got, want := LiveRowsBeforeDelete(seeded)-DeletedLiveRows(), 31; got != want {
		t.Fatalf("live rows after the delete is %d, want %d", got, want)
	}
}
