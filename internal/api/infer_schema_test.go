package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/store"
)

func inferOverWire(t *testing.T, samples []map[string]any) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{"samples": samples})
	if err != nil {
		t.Fatal(err)
	}
	res, err := Ops["infer_schema"].Func(context.Background(), New(nil, nil), body)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTheInferSchemaSchemaDeclaresEveryFieldItReturns(t *testing.T) {
	out := inferOverWire(t, []map[string]any{{"a.b": 1, "a_b": "x", "n": nil}})
	published := Ops["infer_schema"].OutputSchema
	props, _ := published["properties"].(map[string]any)
	required := map[string]bool{}
	for _, name := range published["required"].([]string) {
		required[name] = true
	}
	for field := range out {
		if _, declared := props[field]; !declared {
			t.Fatalf("infer_schema returns %q but its published schema does not declare it, and the schema forbids additional properties: a client validating the response would reject it", field)
		}
		if !required[field] {
			t.Fatalf("infer_schema always returns %q, so the schema must require it", field)
		}
	}
	entry, _ := props["evidence"].(map[string]any)["additionalProperties"].(map[string]any)
	entryProps, _ := entry["properties"].(map[string]any)
	for name, ev := range out["evidence"].(map[string]any) {
		for key := range ev.(map[string]any) {
			if _, declared := entryProps[key]; !declared {
				t.Fatalf("evidence for %q carries %q, which the published schema does not declare", name, key)
			}
		}
	}
}

func TestInferSchemaWarnsWhenTheSamplesCarryMoreFieldsThanATableHolds(t *testing.T) {
	wide := map[string]any{}
	for i := 0; i < store.MaxFieldsPerTable+2; i++ {
		wide[fmt.Sprintf("f%03d", i)] = i
	}
	out := inferOverWire(t, []map[string]any{wide})
	want := fmt.Sprintf("the samples carry %d distinct fields but a table holds at most %d", store.MaxFieldsPerTable+2, store.MaxFieldsPerTable)
	for _, w := range out["warnings"].([]any) {
		if strings.Contains(w.(string), want) {
			return
		}
	}
	t.Fatalf("no warning names the table's field limit: %v", out["warnings"])
}
