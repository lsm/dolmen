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
