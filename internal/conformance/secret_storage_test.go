package conformance

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/store"
)

const plaintextMarker = "PLAINTEXT"

func storageHoldsPlaintext(t *testing.T, h *harness) []string {
	t.Helper()
	if testEngine(t) == store.EnginePostgres {
		return postgresStorageHits(t, h)
	}
	var hits []string
	files, err := filepath.Glob(filepath.Join(h.dir, "*.db*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(plaintextMarker)) {
			hits = append(hits, filepath.Base(f))
		}
	}
	return hits
}

func pgConn(t *testing.T) (*pgx.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	conn, err := pgx.Connect(ctx, postgresDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn, ctx
}

func postgresStorageHits(t *testing.T, h *harness) []string {
	t.Helper()
	conn, ctx := pgConn(t)
	catalog := postgresCatalog(h.dir)
	schemas := []string{catalog}
	rows, err := conn.Query(ctx, "SELECT physical FROM "+pgx.Identifier{catalog}.Sanitize()+".namespaces")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		schemas = append(schemas, p)
	}
	rows.Close()
	type rel struct{ schema, table string }
	var rels []rel
	for _, sc := range schemas {
		rows, err := conn.Query(ctx, "SELECT table_name FROM information_schema.tables WHERE table_schema=$1 AND table_type='BASE TABLE'", sc)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			rels = append(rels, rel{sc, name})
		}
		rows.Close()
	}
	if len(rels) < 3 {
		t.Fatalf("the storage scan found only %d relations; it is not looking where dolmen writes", len(rels))
	}
	needleHex := hex.EncodeToString([]byte(plaintextMarker))
	var hits []string
	for _, r := range rels {
		var n int64
		q := "SELECT count(*) FROM " + pgx.Identifier{r.schema, r.table}.Sanitize() + " AS t WHERE t::text LIKE $1 OR t::text LIKE $2"
		if err := conn.QueryRow(ctx, q, "%"+plaintextMarker+"%", "%"+needleHex+"%").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			hits = append(hits, r.schema+"."+r.table)
		}
	}
	return hits
}

func tamperSecret(t *testing.T, h *harness, ns, table, field string, id int64) {
	t.Helper()
	if testEngine(t) == store.EnginePostgres {
		conn, ctx := pgConn(t)
		catalog := pgx.Identifier{postgresCatalog(h.dir)}.Sanitize()
		var nsPhysical, physical, columnsJSON string
		err := conn.QueryRow(ctx, "SELECT n.physical, t.physical, t.columns_json FROM "+catalog+".tables t JOIN "+catalog+".namespaces n ON n.name=t.namespace WHERE t.namespace=$1 AND t.name=$2 AND t.active", ns, table).Scan(&nsPhysical, &physical, &columnsJSON)
		if err != nil {
			t.Fatal(err)
		}
		var columns map[string]string
		if err := json.Unmarshal([]byte(columnsJSON), &columns); err != nil {
			t.Fatal(err)
		}
		col := pgx.Identifier{columns[field]}.Sanitize()
		if _, err := conn.Exec(ctx, "UPDATE "+pgx.Identifier{nsPhysical, physical}.Sanitize()+" SET "+col+" = overlay("+col+" placing '\\xff'::bytea from length("+col+")) WHERE id=$1", id); err != nil {
			t.Fatal(err)
		}
		return
	}
	db, err := sql.Open("sqlite", filepath.Join(h.dir, ns+".db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	var blob []byte
	if err := db.QueryRowContext(ctx, "SELECT "+field+" FROM "+table+" WHERE id = ?", id).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	blob[len(blob)-1] ^= 0xff
	if _, err := db.ExecContext(ctx, "UPDATE "+table+" SET "+field+" = ? WHERE id = ?", blob, id); err != nil {
		t.Fatal(err)
	}
}

func wantNoPlaintextInStorage(t *testing.T, h *harness) {
	t.Helper()
	if hits := storageHoldsPlaintext(t, h); len(hits) > 0 {
		t.Fatalf("plaintext found in storage: %s", strings.Join(hits, ", "))
	}
}
