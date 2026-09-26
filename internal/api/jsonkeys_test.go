package api

import (
	"encoding/json"
	"reflect"
	"testing"
)

type keyProbeNested struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

type keyProbeInner struct {
	Pointer  *keyProbeNested        `json:"pointer"`
	Items    []keyProbeNested       `json:"items"`
	Rows     []map[string]any       `json:"rows"`
	Freeform map[string]any         `json:"freeform"`
	Raw      json.RawMessage        `json:"raw"`
	Loose    []any                  `json:"loose"`
	Embedded *struct{ Deep string } `json:"embedded"`
	Renamed  string                 `json:"-"`
	Plain    string
	keyProbeNested
}

type keyProbeRequest struct {
	Namespace string        `json:"namespace"`
	Nested    keyProbeInner `json:"nested"`
	Skipped   string        `json:"-"`
	unexposed string
}

func TestUnknownKeysAreMatchedExactly(t *testing.T) {
	known := map[string]any{
		"namespace": "n",
		"nested": map[string]any{
			"pointer":  map[string]any{"label": "l", "count": 1},
			"items":    []any{map[string]any{"label": "l"}, map[string]any{"count": 2}},
			"rows":     []any{map[string]any{"whatever": 1, "Deep": 2}},
			"freeform": map[string]any{"Nested": 1},
			"raw":      map[string]any{"Nested": 1},
			"loose":    []any{map[string]any{"Plain": 1}},
			"embedded": map[string]any{"Deep": "d"},
			"Plain":    "an untagged Go field keeps its own name",
			"label":    "promoted from the embedded struct",
			"count":    "promoted too",
		},
	}
	var req keyProbeRequest
	if err := rejectUnknownKeys(known, &req); err != nil {
		t.Fatalf("every one of these keys is known: %v", err)
	}

	for _, body := range []struct {
		what string
		body map[string]any
		want string
	}{
		{"miscased top level", map[string]any{"Namespace": "n"}, "Namespace"},
		{"miscased nested", map[string]any{"namespace": "n", "nested": map[string]any{"POINTER": 1}}, "POINTER"},
		{"miscased inside a slice element", map[string]any{"namespace": "n", "nested": map[string]any{"items": []any{map[string]any{"label": "l"}, map[string]any{"COUNT": 1}}}}, "COUNT"},
		{"miscased on a promoted field", map[string]any{"namespace": "n", "nested": map[string]any{"Label": "l"}}, "Label"},
		{"miscased inside an anonymous struct", map[string]any{"namespace": "n", "nested": map[string]any{"embedded": map[string]any{"deep": "d"}}}, "deep"},
		{"unknown beside a known one", map[string]any{"namespace": "n", "bogus": 1}, "bogus"},
		{"skipped json name", map[string]any{"skipped": 1}, "skipped"},
		{"untagged Go field", map[string]any{"plain": 1}, "plain"},
		{"promoted field at the top level", map[string]any{"label": "l"}, "label"},
		{"unexported field", map[string]any{"unexposed": 1}, "unexposed"},
	} {
		var req keyProbeRequest
		err := rejectUnknownKeys(body.body, &req)
		uf, ok := err.(*unknownFieldError)
		if !ok {
			t.Errorf("%s: got %v, want an unknown field for %q", body.what, err, body.want)
			continue
		}
		if uf.Field != body.want {
			t.Errorf("%s: reported %q, want %q", body.what, uf.Field, body.want)
		}
	}
}

func TestFieldIndexIsCachedPerType(t *testing.T) {
	if got := fieldIndexOf(reflect.TypeOf(keyProbeRequest{})); got != fieldIndexOf(reflect.TypeOf(keyProbeRequest{})) {
		t.Fatal("the field index must be built once per request type")
	}
}

func TestAProbeValueThatDoesNotFitIsLeftToTheDecoder(t *testing.T) {
	var req keyProbeRequest
	for _, probe := range []any{"string", 1.0, nil, []any{1.0}} {
		if err := rejectUnknownKeys(probe, &req); err != nil {
			t.Errorf("a probe of %T is a type mismatch, not an unknown key: %v", probe, err)
		}
	}
}
