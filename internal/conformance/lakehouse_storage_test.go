package conformance

import (
	"bytes"
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func lakehouseCatalog(t *testing.T, h *harness, ns string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(h.dir, filepath.FromSlash(ns)+".lakehouse", "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func lakehouseStorageHits(t *testing.T, h *harness) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir(h.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.Contains(path, ".lakehouse") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(b, []byte(plaintextMarker)) {
			hits = append(hits, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

func lakehouseSecretBlobs(t *testing.T, h *harness, ns, table, field string) [][]byte {
	t.Helper()
	rows, err := lakehouseCatalog(t, h, ns).QueryContext(context.Background(), `SELECT value FROM _dolmen_lakehouse_secrets WHERE table_name = ? AND field = ?`, table, field)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var blob []byte
		if err := rows.Scan(&blob); err != nil {
			t.Fatal(err)
		}
		out = append(out, blob)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func lakehouseTamperSecret(t *testing.T, h *harness, ns, table, field string, id int64) {
	t.Helper()
	db := lakehouseCatalog(t, h, ns)
	ctx := context.Background()
	var blob []byte
	if err := db.QueryRowContext(ctx, `SELECT value FROM _dolmen_lakehouse_secrets WHERE table_name = ? AND field = ? AND row_id = ?`, table, field, id).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	blob[len(blob)-1] ^= 0xff
	if _, err := db.ExecContext(ctx, `UPDATE _dolmen_lakehouse_secrets SET value = ? WHERE table_name = ? AND field = ? AND row_id = ?`, blob, table, field, id); err != nil {
		t.Fatal(err)
	}
}
