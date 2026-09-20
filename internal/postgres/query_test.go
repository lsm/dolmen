package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestPostgresQueryNamespaceBoundary(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	for _, ns := range []string{"app", "foreign"} {
		if err := s.CreateNamespace(ctx, ns, [16]byte{}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateTable(ctx, ns, "notes", []schema.Field{{Name: "body", Vectorize: true}, {Name: "n", Type: schema.Number}, {Name: "metadata", Type: schema.JSON}}, store.TableOpts{}, [16]byte{}); err != nil {
			t.Fatal(err)
		}
		emb := store.Embedder{Identity: "test", Embed: func(context.Context, []string) ([][]float32, error) { return [][]float32{{1, 0}}, nil }}
		if _, err := s.Insert(ctx, ns, "notes", []map[string]any{{"body": ns, "n": int64(9007199254740993), "metadata": map[string]any{"exact": json.Number("9007199254740993")}}}, store.WriteOpts{}, emb, nil, store.Incarnation{}); err != nil {
			t.Fatal(err)
		}
	}
	result, err := s.Query(ctx, "app", "SELECT * FROM notes WHERE n = ?", []any{json.Number("9007199254740993")}, [16]byte{}, store.Page{})
	if err != nil || len(result.Rows) != 1 || result.Rows[0]["body"] != "app" || result.Rows[0]["n"] != int64(9007199254740993) {
		t.Fatalf("query: %+v %v", result, err)
	}
	if _, ok := result.Rows[0]["_embedding"]; ok {
		t.Fatal("hidden embedding exposed")
	}
	if result.Rows[0]["metadata"].(map[string]any)["exact"] != json.Number("9007199254740993") {
		t.Fatalf("JSON changed: %#v", result.Rows)
	}
	err = s.readOnly(ctx, "app", func(tx pgx.Tx, _ namespace) error {
		var readOnly string
		if err := tx.QueryRow(ctx, "SHOW transaction_read_only").Scan(&readOnly); err != nil {
			return err
		}
		if readOnly != "on" {
			return errors.New("read helper opened a writable transaction")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{"SELECT * FROM pg_catalog.pg_class", "SELECT pg_read_file('/etc/passwd')", "SELECT _embedding FROM notes", "WITH x AS (DELETE FROM notes RETURNING *) SELECT * FROM x", "SELECT current_setting('role')"} {
		if _, err := s.Query(ctx, "app", sql, nil, [16]byte{}, store.Page{}); err == nil {
			t.Errorf("unsafe query accepted: %s", sql)
		}
	}
	if _, err := s.Query(ctx, "app", "SELECT 1 AS duplicate, 2 AS duplicate", nil, [16]byte{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("duplicate label: %v", err)
	}
	if _, err := s.Query(ctx, "app", "SELECT 'NaN'::float4", nil, [16]byte{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("nonfinite result: %v", err)
	}
	err = s.read(ctx, "app", func(tx pgx.Tx, n namespace) error {
		if err := enterQueryRole(ctx, tx, s.queryRole); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "SELECT _embedding FROM "+ident(n.physical, "notes"))
		if err == nil {
			return errors.New("query role can read hidden embeddings")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "after_role", []schema.Field{{Name: "body"}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Query(ctx, "app", "SELECT * FROM after_role", nil, [16]byte{}, store.Page{}); err != nil {
		t.Fatalf("new table not granted: %v", err)
	}
	if _, err := s.Insert(ctx, "app", "after_role", []map[string]any{{"body": "write still works"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatalf("pooled role leaked: %v", err)
	}
}

func TestPostgresQueryLongNamesAndNativeResults(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	long := strings.Repeat("a", 64)
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", long, []schema.Field{{Name: long}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "app", long, []map[string]any{{long: "kept"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "app", "SELECT * FROM "+ident(long), nil, [16]byte{}, store.Page{})
	if err != nil || len(result.Rows) != 1 || result.Rows[0][long] != "kept" {
		t.Fatalf("logical labels: %+v %v", result, err)
	}
	result, err = s.Query(ctx, "app", "WITH q AS (SELECT "+ident(long)+" FROM "+ident(long)+") SELECT "+ident(long)+" FROM q", nil, [16]byte{}, store.Page{})
	if err != nil || result.Rows[0][long] != "kept" {
		t.Fatalf("CTE long names: %+v %v", result, err)
	}
	result, err = s.Query(ctx, "app", `SELECT json_build_object('exact',9007199254740993) AS obj`, nil, [16]byte{}, store.Page{})
	if err != nil || result.Rows[0]["obj"].(map[string]any)["exact"] != json.Number("9007199254740993") {
		t.Fatalf("native JSON: %+v %v", result, err)
	}
	result, err = s.Query(ctx, "app", "SELECT x FROM generate_series(1,5) x ORDER BY x", nil, [16]byte{}, store.Page{Offset: 1, Limit: 2})
	if err != nil || len(result.Rows) != 2 || !result.Truncated || result.Rows[0]["x"] != int64(2) {
		t.Fatalf("pagination: %+v %v", result, err)
	}
	if _, err := s.Query(ctx, "app", "SELECT repeat('x',33*1024*1024) AS body", nil, [16]byte{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("response budget: %v", err)
	}
	if _, err := s.Query(ctx, "app", "SELECT json_build_object('x',repeat('x',33*1024*1024)) AS metadata", nil, [16]byte{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("JSON response budget: %v", err)
	}
}

func TestPostgresQueryClearsStaleGrantGeneration(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	old, err := s.ensureQueryRole(ctx, "app", [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DropNamespace(ctx, "app", old); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	s.rememberQueryGrant("app", old)
	if _, err := s.Query(ctx, "app", "SELECT 1 AS n", nil, [16]byte{}, store.Page{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale generation: %v", err)
	}
	if _, ok := s.queryGrant("app"); ok {
		t.Fatal("stale grant generation remained cached")
	}
	if _, err := s.Query(ctx, "app", "SELECT 1 AS n", nil, [16]byte{}, store.Page{}); err != nil {
		t.Fatalf("query did not recover after stale grant: %v", err)
	}
}
