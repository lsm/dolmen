package postgres

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestPostgresRewriteKeepsUnknownCatalogKeys(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := context.Background()
	if err := s.CreateNamespace(ctx, "keep", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "keep", "t", []schema.Field{{Name: "a", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := s.pool.QueryRow(ctx, "SELECT schema_json FROM "+s.relation("tables")+" WHERE namespace='keep' AND name='t'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.Unmarshal([]byte(raw), &doc)
	doc["future_table_setting"] = "kept"
	doc["fields"].([]any)[0].(map[string]any)["future_field_setting"] = 7.0
	patched, _ := json.Marshal(doc)
	if _, err := s.pool.Exec(ctx, "UPDATE "+s.relation("tables")+" SET schema_json=$1 WHERE namespace='keep' AND name='t'", string(patched)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx, "keep", "t", []schema.Change{
		{Op: schema.OpRenameField, From: "a", To: "b"},
		{Op: schema.OpAddField, Field: &schema.Field{Name: "n", Type: schema.Number}},
	}, store.Embedder{}, store.Incarnation{Version: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT schema_json FROM "+s.relation("tables")+" WHERE namespace='keep' AND name='t'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var after struct {
		Future string           `json:"future_table_setting"`
		Fields []map[string]any `json:"fields"`
	}
	json.Unmarshal([]byte(raw), &after)
	if after.Future != "kept" || after.Fields[0]["name"] != "b" || after.Fields[0]["future_field_setting"] != 7.0 {
		t.Fatalf("unknown catalog keys must survive a migration, including across a rename: %s", raw)
	}
}
