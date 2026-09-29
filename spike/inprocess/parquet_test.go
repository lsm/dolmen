package inprocess

import (
	"path/filepath"
	"testing"
)

func parquetTable(t *testing.T, rows []ParquetRow) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notes.parquet")
	if err := writeParquet(path, rows); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAMinimalTableOverTheParquetReaderScansAndFilters(t *testing.T) {
	useConfinedEngine(t)
	rows := []ParquetRow{
		{ID: 1, Body: "first note", Score: 1.5, Flag: true},
		{ID: 2, Body: "second note", Score: 2.5, Flag: false},
		{ID: 3, Body: "third note", Score: 3.5, Flag: true},
	}
	path := parquetTable(t, rows)

	read, err := readParquet(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(read) != 3 {
		t.Fatalf("parquet reader returned %d rows, want 3", len(read))
	}
	if read[1].Body != "second note" {
		t.Fatalf("row 1 body = %q, want second note", read[1].Body)
	}
	if !read[0].Flag || read[1].Flag {
		t.Fatalf("bool column round-tripped wrong: %v %v", read[0].Flag, read[1].Flag)
	}
	if read[2].Score != 3.5 {
		t.Fatalf("float64 column round-tripped as %v, want 3.5", read[2].Score)
	}

	ns := NewNamespace("app", Options{ReadOnly: true, Locked: true})
	if err := ns.createParquetTable("notes", path); err != nil {
		t.Fatal(err)
	}
	got, err := ns.Query("SELECT id, body FROM notes WHERE id = 2")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("filtered scan over the parquet table returned %d rows, want 1: %v", len(got), got)
	}
	if got[0][1] != "second note" {
		t.Fatalf("body = %v, want second note", got[0][1])
	}
	all, err := ns.Query("SELECT id, body FROM notes ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("unfiltered scan over the parquet table returned %d rows, want 3", len(all))
	}
}
