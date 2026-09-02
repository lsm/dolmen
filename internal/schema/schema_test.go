package schema

import (
	"strings"
	"testing"
)

func TestInferFields(t *testing.T) {
	long := "A very detailed finding body." + strings.Repeat(" x", 150)
	fields := InferFields([]map[string]any{
		{"title": "bug", "score": 3.5, "ok": true, "when": "2026-09-01T10:00:00Z", "detail": long, "tags": []any{"a"}},
		{"title": "task", "score": 1, "ok": false, "when": "2026-09-02", "detail": long},
	})
	byName := map[string]Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	if byName["score"].Type != Number {
		t.Errorf("score: got %s", byName["score"].Type)
	}
	if byName["ok"].Type != Boolean {
		t.Errorf("ok: got %s", byName["ok"].Type)
	}
	if byName["when"].Type != Timestamp {
		t.Errorf("when: got %s", byName["when"].Type)
	}
	if byName["tags"].Type != JSON {
		t.Errorf("tags: got %s", byName["tags"].Type)
	}
	if byName["detail"].Type != Text || !byName["detail"].Fulltext {
		t.Errorf("detail: got %+v", byName["detail"])
	}
	if byName["title"].Type != String {
		t.Errorf("title: got %s", byName["title"].Type)
	}
}

func TestVectorCodecRoundTrip(t *testing.T) {
	in := []float32{1.5, -2.25, 0, 3.75}
	out, err := DecodeVector(EncodeVector(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("length mismatch: %d", len(out))
	}
	for i := range in {
		if in[i] != out[i] {
			t.Fatalf("value %d: %f != %f", i, in[i], out[i])
		}
	}
	if _, err := DecodeVector([]byte{1, 2, 3}); err == nil {
		t.Fatal("expected odd-length blob to fail")
	}
}

func TestValidateRejects(t *testing.T) {
	cases := [][]Field{
		{},
		{{Name: "id", Type: String}},
		{{Name: "Bad", Type: String}},
		{{Name: "a", Type: "bogus"}},
		{{Name: "a", Type: String}, {Name: "a", Type: String}},
		{{Name: "a", Type: Number, Fulltext: true}},
		{{Name: "a", Type: Number, Vectorize: true}},
		{{Name: "a", Type: String, Vectorize: true}, {Name: "b", Type: String, Vectorize: true}},
		{{Name: "a", Type: Vector}},
		{{Name: "a", Type: Vector, Dim: 9999}},
	}
	for i, fields := range cases {
		if err := Validate(fields); err == nil {
			t.Errorf("case %d: expected rejection for %+v", i, fields)
		}
	}
}

func TestValidateAccepts(t *testing.T) {
	fields := []Field{
		{Name: "title", Type: String, Fulltext: true},
		{Name: "body", Type: Text, Vectorize: true},
		{Name: "emb", Type: Vector, Dim: 1536},
	}
	if err := Validate(fields); err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

func TestInferMixedScalarKindsFallbackToJSON(t *testing.T) {
	fields := InferFields([]map[string]any{
		{"v": true, "n": 1},
		{"v": "unknown", "n": "2"},
	})
	byName := map[string]Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	if byName["v"].Type != JSON {
		t.Errorf("v: got %s, want json", byName["v"].Type)
	}
	if byName["n"].Type != JSON {
		t.Errorf("n: got %s, want json", byName["n"].Type)
	}
}

func TestInferStructuredMixedKindsFallbackToJSON(t *testing.T) {
	fields := InferFields([]map[string]any{
		{"v": true},
		{"v": map[string]any{"state": "unknown"}},
	})
	if fields[0].Type != JSON {
		t.Fatalf("scalar+object mix: got %s, want json", fields[0].Type)
	}
}

func TestInferCaseVariantKeysMerge(t *testing.T) {
	fields := InferFields([]map[string]any{
		{"Name": "a"},
		{"name": "b"},
	})
	if len(fields) != 1 || fields[0].Name != "name" {
		t.Fatalf("case variants should merge into one field: %+v", fields)
	}
	if fields[0].Type != String {
		t.Fatalf("merged same-kind variants should keep the kind, got %s", fields[0].Type)
	}
}

func TestInferUppercaseKeyStringNotTimestamp(t *testing.T) {
	fields := InferFields([]map[string]any{
		{"Name": "Alice"},
	})
	if len(fields) != 1 || fields[0].Name != "name" || fields[0].Type != String {
		t.Fatalf("plain name should infer string, got %+v", fields)
	}
}

func TestInferSkipsNulls(t *testing.T) {
	fields := InferFields([]map[string]any{
		{"name": "Alice"},
		{"name": nil},
	})
	if len(fields) != 1 || fields[0].Name != "name" || fields[0].Type != String {
		t.Fatalf("nullable string field should infer string, got %+v", fields)
	}
}

func TestInferAllNullKeyRetained(t *testing.T) {
	fields := InferFields([]map[string]any{
		{"name": nil, "x": 1},
	})
	byName := map[string]Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	if _, ok := byName["name"]; !ok {
		t.Fatalf("all-null key must be retained, got %+v", fields)
	}
	if byName["name"].Type != JSON {
		t.Fatalf("all-null key should infer nullable json, got %s", byName["name"].Type)
	}
}

func TestInferInvalidTimestampsStayString(t *testing.T) {
	for _, bad := range []string{"2026-99-99", "2026-01-01T++++", "2026-02-30"} {
		fields := InferFields([]map[string]any{{"when": bad}})
		if len(fields) != 1 || fields[0].Type != String {
			t.Fatalf("date-shaped but invalid %q must infer string, got %+v", bad, fields)
		}
	}
}

func TestInferValidTimestampVariants(t *testing.T) {
	for _, good := range []string{
		"2026-09-01",
		"2026-09-01T10:00:00Z",
		"2026-09-01T10:00:00.5+02:00",
		"2026-09-01 10:00:00",
		"2026-09-01T10:00:05",
	} {
		fields := InferFields([]map[string]any{{"when": good}})
		if len(fields) != 1 || fields[0].Type != Timestamp {
			t.Fatalf("valid timestamp %q must infer timestamp, got %+v", good, fields)
		}
	}
}

func TestInferOutOfRangeTimestampOffsetsStayString(t *testing.T) {
	for _, bad := range []string{"2026-01-01T00:00:00+24:00", "2026-01-01T00:00:00+23:60", "2026-01-01T00:00:00-25:00"} {
		fields := InferFields([]map[string]any{{"when": bad}})
		if len(fields) != 1 || fields[0].Type != String {
			t.Fatalf("out-of-range offset %q must infer string, got %+v", bad, fields)
		}
	}
	for _, good := range []string{"2026-01-01T00:00:00+23:59", "2026-01-01T00:00:00-05:30", "2026-01-01T00:00:00Z"} {
		fields := InferFields([]map[string]any{{"when": good}})
		if len(fields) != 1 || fields[0].Type != Timestamp {
			t.Fatalf("in-range offset %q must infer timestamp, got %+v", good, fields)
		}
	}
}

func TestInferGoNumericTypesAreNumbers(t *testing.T) {
	fields := InferFields([]map[string]any{
		{"n": float32(1.5), "m": int32(7), "u": uint64(9)},
	})
	byName := map[string]Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	for _, name := range []string{"n", "m", "u"} {
		if byName[name].Type != Number {
			t.Fatalf("go numeric %q should infer number, got %s", name, byName[name].Type)
		}
	}
}

func TestValidateRejectsDimOnNonVectorField(t *testing.T) {
	err := Validate([]Field{{Name: "s", Type: String, Dim: 4}})
	if err == nil {
		t.Fatal("expected dim on a non-vector field to be rejected")
	}
}

func TestInferLowercaseRFC3339Separators(t *testing.T) {
	for _, good := range []string{"2026-09-01t10:00:00z", "2026-09-01T10:00:00z"} {
		fields := InferFields([]map[string]any{{"when": good}})
		if len(fields) != 1 || fields[0].Type != Timestamp {
			t.Fatalf("lowercase rfc3339 %q must infer timestamp, got %+v", good, fields)
		}
	}
}

func TestSQLiteReservedTableNamesRejected(t *testing.T) {
	for _, name := range []string{"sqlite_master", "sqlite_sequence", "sqlite_foo"} {
		if ValidTableName(name) {
			t.Fatalf("sqlite_-prefixed name %q must be rejected", name)
		}
	}
	if !ValidTableName("sqlitey_table") {
		t.Fatal("names that merely start with the letters sqlite must stay valid")
	}
}

func TestInferDefinedNumericTypesAreNumbers(t *testing.T) {
	type score int64
	type ratio float32
	fields := InferFields([]map[string]any{{"s": score(3), "r": ratio(0.5)}})
	byName := map[string]Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	for _, name := range []string{"s", "r"} {
		if byName[name].Type != Number {
			t.Fatalf("defined numeric %q should infer number, got %s", name, byName[name].Type)
		}
	}
}

func TestInferDefinedStringAndBoolTypes(t *testing.T) {
	type label string
	type flag bool
	fields := InferFields([]map[string]any{{"l": label("hello"), "f": flag(true)}})
	byName := map[string]Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	if byName["l"].Type != String {
		t.Fatalf("defined string type should infer string, got %s", byName["l"].Type)
	}
	if byName["f"].Type != Boolean {
		t.Fatalf("defined bool type should infer boolean, got %s", byName["f"].Type)
	}
}

func TestInferNamedStringTimestampDetection(t *testing.T) {
	type when string
	fields := InferFields([]map[string]any{{"w": when("2026-09-01T10:00:00Z")}})
	if len(fields) != 1 || fields[0].Type != Timestamp {
		t.Fatalf("named string timestamp should infer timestamp, got %+v", fields)
	}
}

func TestInferUintptrSamplesAreNumbers(t *testing.T) {
	type handle uintptr
	fields := InferFields([]map[string]any{{"p": uintptr(4), "h": handle(8)}})
	byName := map[string]Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	for _, name := range []string{"p", "h"} {
		if byName[name].Type != Number {
			t.Fatalf("uintptr sample %q should infer number, got %s", name, byName[name].Type)
		}
	}
}

func TestInferTypedNilValuesSkipped(t *testing.T) {
	var missing *string
	fields := InferFields([]map[string]any{
		{"name": "Alice"},
		{"name": missing},
	})
	if len(fields) != 1 || fields[0].Type != String {
		t.Fatalf("typed nil must not turn a string field into json, got %+v", fields)
	}
}
