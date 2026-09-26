package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	ShapeObject       = "object"
	ShapeArray        = "array"
	ShapeArrayString  = "array<string>"
	ShapeArrayNumber  = "array<number>"
	ShapeArrayBoolean = "array<boolean>"
	ShapeArrayObject  = "array<object>"
)

var Shapes = []string{ShapeObject, ShapeArray, ShapeArrayString, ShapeArrayNumber, ShapeArrayBoolean, ShapeArrayObject}

var shapeExamples = map[string]string{
	ShapeObject:       `{"key":"value"}`,
	ShapeArray:        `[1,"two"]`,
	ShapeArrayString:  `["a","b"]`,
	ShapeArrayNumber:  `[1,2.5]`,
	ShapeArrayBoolean: `[true,false]`,
	ShapeArrayObject:  `[{"key":"value"}]`,
}

var shapeElements = map[string]string{
	ShapeArrayString:  "string",
	ShapeArrayNumber:  "number",
	ShapeArrayBoolean: "boolean",
	ShapeArrayObject:  "object",
}

func ValidateShape(field, shape string) error {
	if _, ok := shapeExamples[shape]; ok {
		return nil
	}
	return fmt.Errorf("field %q: unknown shape %q (valid: %s)", field, shape, strings.Join(Shapes, ", "))
}

func ShapeViolation(field, shape string, raw []byte) error {
	if shape == "" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("field %q: value is not valid JSON: %w", field, err)
	}
	if v == nil {
		return nil
	}
	got, ok := shapeMismatch(shape, v)
	if ok {
		return nil
	}
	return fmt.Errorf("field %q expects %s but got %s; send it as %s, e.g. %s", field, shape, got, shapeNoun(shape), shapeExamples[shape])
}

func shapeNoun(shape string) string {
	switch shape {
	case ShapeObject:
		return "a JSON object"
	case ShapeArray:
		return "a JSON array"
	}
	return "a JSON array of " + shapeElements[shape] + "s"
}

func shapeMismatch(shape string, v any) (string, bool) {
	if shape == ShapeObject {
		if shapeKind(v) == "object" {
			return "", true
		}
		return article(shapeKind(v)), false
	}
	items, ok := v.([]any)
	if !ok {
		return article(shapeKind(v)), false
	}
	want := shapeElements[shape]
	if want == "" {
		return "", true
	}
	for i, item := range items {
		if kind := shapeKind(item); kind != want {
			return fmt.Sprintf("an array holding %s at index %d", article(kind), i), false
		}
	}
	return "", true
}

func article(kind string) string {
	switch kind {
	case "null":
		return "null"
	case "object", "array":
		return "an " + kind
	}
	return "a " + kind
}

func shapeKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	}
	return fmt.Sprintf("%T", v)
}
