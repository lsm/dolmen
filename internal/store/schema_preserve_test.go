package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func TestARewriteKeepsCatalogKeysThisBinaryDoesNotKnow(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateNotes(t, st)
	n, err := st.ns("test")
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := n.rw.QueryRow(`SELECT schema_json FROM _dolmen_tables WHERE name = 'notes'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.Unmarshal([]byte(raw), &doc)
	doc["future_table_setting"] = "kept"
	for _, f := range doc["fields"].([]any) {
		if f.(map[string]any)["name"] == "title" {
			f.(map[string]any)["future_field_setting"] = 7.0
		}
	}
	patched, _ := json.Marshal(doc)
	if _, err := n.rw.Exec(`UPDATE _dolmen_tables SET schema_json = ? WHERE name = 'notes'`, string(patched)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Migrate(ctx, "test", "notes", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "priority", Type: schema.Number}},
		{Op: schema.OpSetFulltext, Name: "title", Value: boolPtr(false)},
	}, testEmbed, 1); err != nil {
		t.Fatal(err)
	}
	if err := n.rw.QueryRow(`SELECT schema_json FROM _dolmen_tables WHERE name = 'notes'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var after map[string]any
	json.Unmarshal([]byte(raw), &after)
	if after["future_table_setting"] != "kept" {
		t.Fatalf("an unknown table key was dropped on rewrite: %s", raw)
	}
	for _, f := range after["fields"].([]any) {
		m := f.(map[string]any)
		if m["name"] != "title" {
			continue
		}
		if m["future_field_setting"] != 7.0 {
			t.Fatalf("an unknown field key was dropped on rewrite: %s", raw)
		}
		if _, still := m["fulltext"]; still {
			t.Fatalf("a known key this binary cleared must stay cleared, not be restored from the old record: %s", raw)
		}
	}
}

func TestARenamedFieldKeepsItsUnknownKeys(t *testing.T) {
	prev := `{"name":"t","fields":[{"name":"a","type":"string","future":1},{"name":"b","type":"string","other":2}]}`
	next := []byte(`{"name":"t","fields":[{"name":"c","type":"string"},{"name":"b","type":"string"}]}`)
	renames := RenamesOf([]schema.Change{{Op: schema.OpRenameField, From: "a", To: "x"}, {Op: schema.OpRenameField, From: "x", To: "c"}})
	got, err := MergeUnknownSchemaKeys(prev, next, renames)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Fields []map[string]any `json:"fields"`
	}
	json.Unmarshal([]byte(got), &doc)
	if doc.Fields[0]["future"] != 1.0 || doc.Fields[1]["other"] != 2.0 {
		t.Fatalf("unknown keys must follow a field through a chain of renames: %s", got)
	}
}

func TestAFieldDroppedAndAddedAgainStartsClean(t *testing.T) {
	prev := `{"name":"t","fields":[{"name":"a","type":"string","future":1}]}`
	next := []byte(`{"name":"t","fields":[{"name":"a","type":"number"}]}`)
	renames := RenamesOf([]schema.Change{{Op: schema.OpDropField, Name: "a"}, {Op: schema.OpAddField, Field: &schema.Field{Name: "a", Type: schema.Number}}})
	got, err := MergeUnknownSchemaKeys(prev, next, renames)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(next) {
		t.Fatalf("a new field must not inherit a dropped field's keys because they share a name: %s", got)
	}
}

func TestARenameOntoADroppedNameCarriesTheRenamedField(t *testing.T) {
	r := RenamesOf([]schema.Change{{Op: schema.OpDropField, Name: "b"}, {Op: schema.OpRenameField, From: "a", To: "b"}})
	if r["b"] != "a" {
		t.Fatalf("b now holds a's data, so it must carry a's keys: %v", r)
	}
	if _, stillA := r["a"]; stillA {
		t.Fatalf("a no longer exists after the rename: %v", r)
	}
}
