package postgres_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen"
	"github.com/lsm/dolmen/postgres"
)

func testConfig(t *testing.T) postgres.Config {
	t.Helper()
	dsn := os.Getenv("DOLMEN_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set DOLMEN_TEST_PG_DSN to exercise the PostgreSQL facade")
	}
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	catalog := "dolmen_facade_" + hex.EncodeToString(id[:])
	t.Cleanup(func() { dropCatalog(t, dsn, catalog) })
	return postgres.Config{DSN: dsn, Catalog: catalog, QueryRole: os.Getenv("DOLMEN_TEST_PG_QUERY_ROLE")}
}

func dropCatalog(t *testing.T, dsn, catalog string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Error(err)
		return
	}
	defer conn.Close(ctx)
	schemas := []string{}
	rows, err := conn.Query(ctx, "SELECT physical FROM "+pgx.Identifier{catalog}.Sanitize()+".namespaces")
	if err == nil {
		for rows.Next() {
			var physical string
			if err := rows.Scan(&physical); err != nil {
				t.Error(err)
				break
			}
			schemas = append(schemas, physical)
		}
		rows.Close()
	}
	schemas = append(schemas, catalog)
	for _, name := range schemas {
		if _, err := conn.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{name}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
	}
}

func TestFacadeServesPostgres(t *testing.T) {
	st, err := dolmen.Open("", postgres.With(testConfig(t)))
	if err != nil {
		t.Fatalf("open with the postgres engine: %v", err)
	}
	defer st.Close()
	ctx := t.Context()
	if err := st.CreateNamespace(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTable(ctx, "app", "notes", []dolmen.Field{{Name: "title"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Insert(ctx, "app", "notes", []map[string]any{{"title": "through the facade"}}, dolmen.InsertOptions{}); err != nil {
		t.Fatal(err)
	}
	result, err := st.Query(ctx, "app", "SELECT title FROM notes", dolmen.QueryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0]["title"] != "through the facade" {
		t.Fatalf("facade query over postgres returned %+v", result.Rows)
	}
}

func TestFacadeRejectsADuplicateCatalog(t *testing.T) {
	cfg := testConfig(t)
	first, err := dolmen.Open("", postgres.With(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := dolmen.Open("", postgres.With(cfg)); err == nil {
		t.Fatal("the same catalog must not be opened twice in one process")
	}
}

func TestFacadeRequiresADSN(t *testing.T) {
	if _, err := dolmen.Open("", postgres.With(postgres.Config{})); err == nil {
		t.Fatal("an empty DSN must be rejected")
	}
}
