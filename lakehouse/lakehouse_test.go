package lakehouse_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/lsm/dolmen"
	"github.com/lsm/dolmen/lakehouse"
)

func TestTheLibraryOpensALakehouseDataDirectory(t *testing.T) {
	dir := t.TempDir()
	st, err := dolmen.Open(dir, lakehouse.With(lakehouse.Config{}))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.CreateNamespace(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTable(ctx, "app", "docs", []dolmen.Field{{Name: "title", Type: dolmen.String}, {Name: "doc", Type: dolmen.File}}); err != nil {
		t.Fatal(err)
	}
	res, err := st.Insert(ctx, "app", "docs", []map[string]any{{"title": "a", "doc": dolmen.FileUpload{Reader: strings.NewReader("hello"), Name: "a.txt"}}}, dolmen.InsertOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.GetRows(ctx, "app", "docs", res.Ids)
	if err != nil || len(rows.Rows) != 1 || rows.Rows[0]["title"] != "a" {
		t.Fatalf("GetRows = %v, %v", rows.Rows, err)
	}
	f, info, err := st.OpenFile(ctx, "app", "docs", res.Ids[0], "doc")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(f)
	f.Close()
	if string(got) != "hello" || info.MediaType != "text/plain" {
		t.Fatalf("OpenFile = %q %+v", got, info)
	}
	if _, err := st.Query(ctx, "app", "SELECT 1", dolmen.QueryOptions{}); !errors.Is(err, dolmen.ErrSQLEngineUnavailable) || !strings.Contains(err.Error(), "sidecar") {
		t.Fatalf("Query without a sidecar: %v", err)
	}
	if _, err := dolmen.Open(dir); !errors.Is(err, dolmen.ErrConflict) {
		t.Fatalf("a second store on the same directory: %v", err)
	}
}

func TestTheLakehouseEngineNamesWhatItNeeds(t *testing.T) {
	if _, err := dolmen.Open(t.TempDir(), dolmen.WithEngine("lakehouse")); !errors.Is(err, dolmen.ErrInvalidRequest) || !strings.Contains(err.Error(), "lakehouse.With") {
		t.Fatalf("WithEngine(lakehouse) alone: %v", err)
	}
	if _, err := dolmen.Open("", lakehouse.With(lakehouse.Config{})); !errors.Is(err, dolmen.ErrInvalidRequest) {
		t.Fatalf("no data directory: %v", err)
	}
}
