package conformance

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/lsm/dolmen/internal/lakehouse"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type lakehouseQueryEngine interface {
	lakehouseAppendEngine
	Query(context.Context, string, string, []any, [16]byte, store.Page) (store.QueryResult, error)
	Capabilities() store.EngineCapabilities
}

func lakehouseSQLEngine(t *testing.T) lakehouse.SQLEngine {
	t.Helper()
	bin := os.Getenv("DOLMEN_TEST_DUCKDB_SIDECAR")
	if bin == "" {
		if os.Getenv("DOLMEN_TEST_DUCKDB_REQUIRED") == "1" {
			t.Fatal("DOLMEN_TEST_DUCKDB_SIDECAR is required in this job")
		}
		t.Skip("DOLMEN_TEST_DUCKDB_SIDECAR not set; lakehouse query runs in the dolmen-duckdb sidecar")
	}
	return lakehouse.SQLEngine{Binary: bin, ExtensionDir: os.Getenv("DOLMEN_TEST_DUCKDB_EXTENSIONS")}
}

func TestLakehouseQueryBackendConformance(t *testing.T) {
	for _, backend := range []string{"sqlite", "lakehouse"} {
		t.Run(backend, func(t *testing.T) {
			var raw namespaceEngine
			var err error
			dialect := store.DialectSQLite
			if backend == "lakehouse" {
				raw, err = lakehouse.Open(t.TempDir(), lakehouse.WithSQLEngine(lakehouseSQLEngine(t)))
				dialect = lakehouse.DialectDuckDB
			} else {
				raw, err = store.Open(t.TempDir())
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { raw.Close() })
			eng, ok := raw.(lakehouseQueryEngine)
			if !ok {
				t.Fatal("engine has no query path")
			}
			if caps := eng.Capabilities(); caps.QueryDialect != dialect || caps.FilterDialect != dialect {
				t.Fatalf("capabilities must name the %s dialect: %+v", dialect, caps)
			}
			ctx := t.Context()
			if err := eng.CreateNamespace(ctx, "ns", [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.CreateTable(ctx, "ns", "notes", []schema.Field{{Name: "title", Type: schema.String}, {Name: "score", Type: schema.Number}}, store.TableOpts{}, [16]byte{}); err != nil {
				t.Fatal(err)
			}
			if _, err := eng.Insert(ctx, "ns", "notes", []map[string]any{{"title": "a", "score": 1.5}, {"title": "b", "score": 7}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
				t.Fatal(err)
			}
			res, err := eng.Query(ctx, "ns", "SELECT id, title FROM notes WHERE title <> ? ORDER BY id", []any{"a"}, [16]byte{}, store.Page{})
			if err != nil || len(res.Rows) != 1 || res.Rows[0]["id"] != int64(2) || res.Rows[0]["title"] != "b" {
				t.Fatalf("a bound filter reads the committed row: %+v %v", res, err)
			}
			page, err := eng.Query(ctx, "ns", "SELECT id FROM notes ORDER BY id", nil, [16]byte{}, store.Page{Limit: 1})
			if err != nil || len(page.Rows) != 1 || !page.Truncated {
				t.Fatalf("a page short of the rows is truncated: %+v %v", page, err)
			}
			if _, err := eng.Insert(ctx, "ns", "notes", []map[string]any{{"title": "huge", "score": 5e18}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
				t.Fatal(err)
			}
			huge, err := eng.Query(ctx, "ns", "SELECT score FROM notes WHERE title = ?", []any{"huge"}, [16]byte{}, store.Page{})
			if err != nil || len(huge.Rows) != 1 || huge.Rows[0]["score"] != int64(5e18) {
				t.Fatalf("an integral double in int64 range reads back as that integer: %#v %v", huge.Rows, err)
			}
			if _, err := eng.Query(ctx, "ns", "DELETE FROM notes", nil, [16]byte{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
				t.Fatalf("a write must be refused as invalid: %v", err)
			}
		})
	}
}
