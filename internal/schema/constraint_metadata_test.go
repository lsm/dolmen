package schema

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestConstraintMetadataRoundTrip(t *testing.T) {
	original := TableSchema{Namespace: "app", Name: "notes", Version: 1,
		Fields: []Field{{Name: "slug", Type: String}, {Name: "parent_id", Type: Number, Default: json.Number("9007199254740993")}},
		Constraints: []Constraint{
			{Name: "live_slug", Kind: ConstraintUnique, Fields: []string{"slug"}, Where: "parent_id IS NULL"},
			{Name: "parent_ref", Kind: ConstraintForeignKey, Field: "parent_id", References: &ConstraintReference{Table: "parents", Field: "id"}},
		}, AcceptReferences: true,
	}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var got TableSchema
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, original) {
		t.Fatalf("metadata changed: %+v", got)
	}
}

func TestEmptyConstraintMetadataKeepsLegacyJSON(t *testing.T) {
	for _, cs := range [][]Constraint{nil, {}} {
		raw, err := json.Marshal(TableSchema{Namespace: "app", Name: "notes", Version: 1, Fields: []Field{}, Constraints: cs})
		if err != nil {
			t.Fatal(err)
		}
		const want = `{"namespace":"app","name":"notes","version":1,"fields":[]}`
		if string(raw) != want {
			t.Fatalf("legacy JSON changed: %s", raw)
		}
	}
}
