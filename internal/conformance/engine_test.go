package conformance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen"
	"github.com/lsm/dolmen/internal/postgres"
	"github.com/lsm/dolmen/internal/store"
	pgfacade "github.com/lsm/dolmen/postgres"
)

func openEngineStore(t *testing.T, dir string, retention *time.Duration) store.Engine {
	t.Helper()
	if testEngine(t) == store.EnginePostgres {
		return openPostgresEngine(t, dir, retention)
	}
	opts := []store.OpenOption{}
	if retention != nil {
		opts = append(opts, store.WithChangeRetention(*retention))
	}
	st, err := store.Open(dir, opts...)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return st
}

func postgresDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("DOLMEN_TEST_PG_DSN")
	if dsn == "" {
		if os.Getenv("DOLMEN_TEST_PG_REQUIRED") == "1" {
			t.Fatal("PostgreSQL CI requires DOLMEN_TEST_PG_DSN")
		}
		t.Skip("set DOLMEN_TEST_PG_DSN for PostgreSQL conformance")
	}
	return dsn
}

func postgresCatalog(dir string) string {
	sum := sha256.Sum256([]byte(dir))
	return "dolmen_conf_" + hex.EncodeToString(sum[:12])
}

var (
	pgCleanupMu   sync.Mutex
	pgCleanupSeen = map[string]bool{}
)

func cleanPostgresCatalog(t *testing.T, dsn, catalog string) {
	t.Helper()
	pgCleanupMu.Lock()
	fresh := !pgCleanupSeen[catalog]
	pgCleanupSeen[catalog] = true
	pgCleanupMu.Unlock()
	if !fresh {
		return
	}
	t.Cleanup(func() {
		pgCleanupMu.Lock()
		delete(pgCleanupSeen, catalog)
		pgCleanupMu.Unlock()
		dropPostgresCatalog(t, dsn, catalog)
	})
}

func postgresFacadeOption(t *testing.T, dir string) dolmen.Option {
	t.Helper()
	dsn := postgresDSN(t)
	catalog := postgresCatalog(dir)
	cleanPostgresCatalog(t, dsn, catalog)
	return pgfacade.With(pgfacade.Config{DSN: dsn, Catalog: catalog, QueryRole: os.Getenv("DOLMEN_TEST_PG_QUERY_ROLE")})
}

func openPostgresEngine(t *testing.T, dir string, retention *time.Duration) *postgres.Store {
	t.Helper()
	dsn := postgresDSN(t)
	catalog := postgresCatalog(dir)
	cfg := postgres.Config{DSN: dsn, Catalog: catalog, QueryRole: os.Getenv("DOLMEN_TEST_PG_QUERY_ROLE"), ChangeRetention: retention}
	s, err := postgres.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	cleanPostgresCatalog(t, dsn, catalog)
	t.Cleanup(func() { s.Close() })
	return s
}

func dropPostgresCatalog(t *testing.T, dsn, catalog string) {
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

func resolveEngine(getenv func(string) string) (string, error) {
	name := getenv("DOLMEN_ENGINE")
	if err := store.ValidateEngine(name); err != nil {
		return "", err
	}
	if name == "" {
		name = store.EngineSQLite
	}
	return name, nil
}

var activeEngine, activeEngineErr = resolveEngine(os.Getenv)

func testEngine(t *testing.T) string {
	t.Helper()
	if activeEngineErr != nil {
		t.Fatalf("DOLMEN_ENGINE: %v", activeEngineErr)
	}
	return activeEngine
}

func sqliteOnly(t *testing.T) {
	t.Helper()
	if name := testEngine(t); name != store.EngineSQLite {
		t.Skipf("engine %q: this fixture probes SQLite storage internals directly", name)
	}
}

func TestEngineKnobResolution(t *testing.T) {
	cases := []struct {
		env     map[string]string
		want    string
		wantErr string
	}{
		{env: nil, want: store.EngineSQLite},
		{env: map[string]string{"DOLMEN_ENGINE": ""}, want: store.EngineSQLite},
		{env: map[string]string{"DOLMEN_ENGINE": "sqlite"}, want: store.EngineSQLite},
		{env: map[string]string{"DOLMEN_ENGINE": "postgres"}, want: store.EnginePostgres},
		{env: map[string]string{"DOLMEN_ENGINE": "banana"}, wantErr: `unknown engine "banana" (available engines are "postgres" and "sqlite")`},
	}
	for _, c := range cases {
		lookup := c.env
		got, err := resolveEngine(func(string) string {
			if lookup == nil {
				return ""
			}
			return lookup["DOLMEN_ENGINE"]
		})
		if c.wantErr != "" {
			if err == nil || err.Error() != c.wantErr {
				t.Fatalf("DOLMEN_ENGINE=%q: error %v, want %q", c.env["DOLMEN_ENGINE"], err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("DOLMEN_ENGINE=%q: %v", c.env["DOLMEN_ENGINE"], err)
		}
		if got != c.want {
			t.Fatalf("DOLMEN_ENGINE=%q: engine %q, want %q", c.env["DOLMEN_ENGINE"], got, c.want)
		}
	}
}
