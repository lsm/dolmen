package api

import (
	"errors"
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
	Raw      map[string]any         `json:"raw"`
	Loose    []any                  `json:"loose"`
	Embedded *struct{ Deep string } `json:"embedded"`
	Skipped  string                 `json:"-"`
	Plain    string
	keyProbeNested
}

type keyProbeRequest struct {
	Namespace string        `json:"namespace"`
	Nested    keyProbeInner `json:"nested"`
	Skipped   string        `json:"-"`
	unexposed string
}

func probeFor(t *testing.T, body string) *jsonObject {
	t.Helper()
	obj, err := probeObject([]byte(body))
	if err != nil {
		t.Fatalf("probe %s: %v", body, err)
	}
	return obj
}

func TestUnknownKeysAreMatchedExactly(t *testing.T) {
	known := `{"namespace":"n","nested":{` +
		`"pointer":{"label":"l","count":1},` +
		`"items":[{"label":"l"},{"count":2}],` +
		`"rows":[{"whatever":1,"Deep":2}],` +
		`"freeform":{"Nested":1},` +
		`"raw":{"Nested":1},` +
		`"loose":[{"Plain":1}],` +
		`"embedded":{"Deep":"d"},` +
		`"Plain":"an untagged Go field keeps its own name",` +
		`"label":"promoted from the embedded struct",` +
		`"count":"promoted too"}}`
	var req keyProbeRequest
	if err := rejectUnknownKeys(probeFor(t, known), &req); err != nil {
		t.Fatalf("every one of these keys is known: %v", err)
	}

	for _, body := range []struct {
		what string
		body string
		want string
	}{
		{"miscased top level", `{"Namespace":"n"}`, "Namespace"},
		{"miscased nested", `{"namespace":"n","nested":{"POINTER":1}}`, "POINTER"},
		{"miscased inside a slice element", `{"namespace":"n","nested":{"items":[{"label":"l"},{"COUNT":1}]}}`, "COUNT"},
		{"miscased on a promoted field", `{"namespace":"n","nested":{"Label":"l"}}`, "Label"},
		{"miscased inside an anonymous struct", `{"namespace":"n","nested":{"embedded":{"deep":"d"}}}`, "deep"},
		{"unknown beside a known one", `{"namespace":"n","bogus":1}`, "bogus"},
		{"skipped json name", `{"skipped":1}`, "skipped"},
		{"untagged Go field", `{"plain":1}`, "plain"},
		{"promoted field at the top level", `{"label":"l"}`, "label"},
		{"unexported field", `{"unexposed":1}`, "unexposed"},
		{"first unknown key in document order wins", `{"Sql":1,"Bogus":2,"sql":3}`, "Sql"},
		{"document order inside a nested object", `{"namespace":"n","nested":{"Zeta":1,"Alpha":2}}`, "Zeta"},
		{"document order inside a slice element", `{"namespace":"n","nested":{"items":[{"Zeta":1},{"Alpha":2}]}}`, "Zeta"},
	} {
		var req keyProbeRequest
		err := rejectUnknownKeys(probeFor(t, body.body), &req)
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

func TestAnUnknownKeyIsAlwaysTheSameOne(t *testing.T) {
	body := `{"Sql":1,"Bogus":2,"nonsense":[1,2,3]}`
	var first string
	for range 50 {
		var req keyProbeRequest
		err := rejectUnknownKeys(probeFor(t, body), &req)
		uf, ok := err.(*unknownFieldError)
		if !ok {
			t.Fatalf("got %v, want an unknown field", err)
		}
		if first == "" {
			first = uf.Field
		}
		if uf.Field != first {
			t.Fatalf("the same body reported %q then %q; the message must be stable", first, uf.Field)
		}
	}
}

func TestTheFirstNullIsAlwaysTheSameOne(t *testing.T) {
	body := `{"namespace":null,"table":null}`
	var first string
	for range 50 {
		err := rejectNulls("", probeFor(t, body))
		if err == nil {
			t.Fatal("a null must be refused")
		}
		if first == "" {
			first = err.Error()
		}
		if err.Error() != first {
			t.Fatalf("the same body reported %q then %q; the message must be stable", first, err.Error())
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
	for _, body := range []string{`"string"`, `1.0`, `null`, `[1.0]`, `{"namespace":"n","nested":[]}`} {
		probe, err := probeObject([]byte(body))
		if err != nil {
			continue
		}
		if err := rejectUnknownKeys(probe, &req); err != nil {
			t.Errorf("probe %s: a value that does not fit is a type mismatch, not an unknown key: %v", body, err)
		}
	}
}

func TestTheProbeRefusesWhatIsNotAnObject(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`null`, "request body must be a JSON object, but the request sent null"},
		{`[1,2]`, "request body must be a JSON object, but the request sent an array"},
		{`"x"`, "request body must be a JSON object, but the request sent a string"},
		{`3`, "request body must be a JSON object, but the request sent a number"},
		{`true`, "request body must be a JSON object, but the request sent a boolean"},
	} {
		_, err := probeObject([]byte(tc.body))
		if err == nil {
			t.Fatalf("%s must not decode as an object", tc.body)
		}
		var apiErr *Error
		if !errors.As(err, &apiErr) || apiErr.Message != tc.want {
			t.Errorf("%s: %v, want %q", tc.body, err, tc.want)
		}
	}
}

func TestTheProbeKeepsTheBodyObject(t *testing.T) {
	for _, body := range []string{`{}`, ` `, ``, "\n\t"} {
		probe, err := probeObject([]byte(body))
		if err != nil {
			t.Fatalf("body %q: %v", body, err)
		}
		if len(probe.keys) != 0 {
			t.Errorf("body %q must be an empty object, got %v", body, probe.keys)
		}
	}
}
