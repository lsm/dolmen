package blackbox

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestStage12TheServedEngineIsTheRequestedOne(t *testing.T) {
	segments := strings.Split(scenarioNamespace, "/")
	segments[len(segments)-1] += ".db"
	sqliteFile := filepath.Join(append([]string{app.srv.dataDir}, segments...)...)
	_, statErr := os.Stat(sqliteFile)

	if app.engine != enginePostgres {
		if statErr != nil {
			t.Fatalf("the SQLite run left no namespace file at %s: %v", sqliteFile, statErr)
		}
		return
	}

	if statErr == nil {
		t.Fatalf("this run asked for PostgreSQL but the binary wrote %s, so every stage above exercised SQLite and proved nothing about PostgreSQL", sqliteFile)
	}
	if !os.IsNotExist(statErr) {
		t.Fatalf("stat %s: %v", sqliteFile, statErr)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, app.pgDSN)
	if err != nil {
		t.Fatalf("reach the catalog the run was pointed at: %v", err)
	}
	defer conn.Close(ctx)
	var present bool
	query := "SELECT EXISTS(SELECT 1 FROM " + pgx.Identifier{app.pgCatalog}.Sanitize() + ".namespaces WHERE name=$1)"
	if err := conn.QueryRow(ctx, query, scenarioNamespace).Scan(&present); err != nil {
		t.Fatalf("read the run's catalog %s: %v", app.pgCatalog, err)
	}
	if !present {
		t.Fatalf("catalog %s holds no %q namespace, so the scenario did not land in the catalog the flags named", app.pgCatalog, scenarioNamespace)
	}
}
