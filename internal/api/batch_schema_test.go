package api

import (
	"reflect"
	"sort"
	"testing"

	"github.com/lsm/dolmen/internal/store"
)

func batchWriteEntry(t *testing.T, k store.BatchWriteKind) map[string]any {
	t.Helper()
	for _, alt := range batchAlternatives(t) {
		if alt.kind == string(k) {
			return alt.entry
		}
	}
	t.Fatalf("batch advertises no write schema for kind %q", k)
	return nil
}

func batchResultEntry(t *testing.T, k store.BatchWriteKind) map[string]any {
	t.Helper()
	for _, alt := range batchResultAlternatives(t) {
		if alt.kind == string(k) {
			return alt.entry
		}
	}
	t.Fatalf("batch advertises no result schema for kind %q", k)
	return nil
}

type batchAlt struct {
	kind  string
	entry map[string]any
}

func batchAlternatives(t *testing.T) []batchAlt {
	t.Helper()
	writes, _ := Ops["batch"].InputSchema["properties"].(map[string]any)
	items, _ := writes["writes"].(map[string]any)
	oneOf, _ := items["items"].(map[string]any)
	var out []batchAlt
	for _, alt := range oneOf["anyOf"].([]any) {
		entry := alt.(map[string]any)
		props := entry["properties"].(map[string]any)
		kind, _ := props["kind"].(map[string]any)
		enum, _ := kind["enum"].([]any)
		if len(enum) == 1 {
			out = append(out, batchAlt{entry: entry, kind: enum[0].(string)})
		}
	}
	return out
}

func batchResultAlternatives(t *testing.T) []batchAlt {
	t.Helper()
	outSchemaTop, _ := Ops["batch"].OutputSchema["properties"].(map[string]any)
	results, _ := outSchemaTop["results"].(map[string]any)
	oneOf, _ := results["items"].(map[string]any)
	var out []batchAlt
	for _, alt := range oneOf["anyOf"].([]any) {
		entry := alt.(map[string]any)
		props := entry["properties"].(map[string]any)
		kind, _ := props["kind"].(map[string]any)
		enum, _ := kind["enum"].([]any)
		if len(enum) == 1 {
			out = append(out, batchAlt{entry: entry, kind: enum[0].(string)})
		}
	}
	return out
}

func batchWriteInput(t *testing.T, k store.BatchWriteKind) map[string]any {
	writes, _ := Ops["batch"].InputSchema["properties"].(map[string]any)
	items, _ := writes["writes"].(map[string]any)
	oneOf, _ := items["items"].(map[string]any)
	for _, alt := range oneOf["anyOf"].([]any) {
		entry := alt.(map[string]any)
		props := entry["properties"].(map[string]any)
		kind, _ := props["kind"].(map[string]any)
		enum, _ := kind["enum"].([]any)
		if len(enum) == 1 && enum[0] == string(k) {
			return props
		}
	}
	t.Fatalf("batch advertises no write schema for kind %q", k)
	return nil
}

func batchWriteResult(t *testing.T, k store.BatchWriteKind) map[string]any {
	out, _ := Ops["batch"].OutputSchema["properties"].(map[string]any)
	results, _ := out["results"].(map[string]any)
	oneOf, _ := results["items"].(map[string]any)
	for _, alt := range oneOf["anyOf"].([]any) {
		entry := alt.(map[string]any)
		props := entry["properties"].(map[string]any)
		kind, _ := props["kind"].(map[string]any)
		enum, _ := kind["enum"].([]any)
		if len(enum) == 1 && enum[0] == string(k) {
			return props
		}
	}
	t.Fatalf("batch advertises no result schema for kind %q", k)
	return nil
}

func advertisedRequired(t *testing.T, kind store.BatchWriteKind, fromInput bool, drop func(string) bool) []string {
	t.Helper()
	var raw []string
	if fromInput {
		in, _ := Ops[string(kind)].InputSchema["required"].([]string)
		raw = in
	} else {
		out, _ := Ops[string(kind)].OutputSchema["required"].([]string)
		raw = out
	}
	out := make([]string, 0, len(raw)+1)
	for _, name := range raw {
		if drop(name) {
			continue
		}
		out = append(out, name)
	}
	out = append(out, "kind")
	sort.Strings(out)
	return out
}

func requiredOf(t *testing.T, s map[string]any) []string {
	t.Helper()
	raw, ok := s["required"].([]string)
	if !ok {
		t.Fatalf("schema carries no []string required list: %#v", s["required"])
	}
	out := append([]string(nil), raw...)
	sort.Strings(out)
	return out
}

func names(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestBatchWriteSchemasAreDerivedFromTheAdvertisedOnes(t *testing.T) {
	for _, kind := range []store.BatchWriteKind{
		store.BatchWriteInsert, store.BatchWriteUpdate, store.BatchWriteDelete,
		store.BatchWriteUpsert, store.BatchWriteUpsertByKey,
	} {
		advertised, ok := Ops[string(kind)].InputSchema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s advertises no input properties", kind)
		}
		want := make([]string, 0, len(advertised)+1)
		for name := range advertised {
			if isBatchPerBatchField(name) {
				continue
			}
			want = append(want, name)
		}
		want = append(want, "kind")
		sort.Strings(want)

		got := names(batchWriteInput(t, kind))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("batch's %s write advertises %v, want the %s input minus the fields a batch sets itself, plus kind: %v", kind, got, kind, want)
		}
		entry := batchWriteEntry(t, kind)
		gotRequired := requiredOf(t, entry)
		wantRequired := advertisedRequired(t, kind, true, isBatchPerBatchField)
		if !reflect.DeepEqual(gotRequired, wantRequired) {
			t.Fatalf("batch's %s write requires %v, want the advertised required list minus the per-batch fields, plus kind: %v", kind, gotRequired, wantRequired)
		}
		for name, def := range batchWriteInput(t, kind) {
			if name == "kind" {
				continue
			}
			if !reflect.DeepEqual(def, advertised[name]) {
				t.Fatalf("batch's %s write changed the advertised definition of %q", kind, name)
			}
		}
	}
}

func TestBatchResultSchemasAreDerivedFromTheAdvertisedOnes(t *testing.T) {
	for _, kind := range []store.BatchWriteKind{
		store.BatchWriteInsert, store.BatchWriteUpdate, store.BatchWriteDelete,
		store.BatchWriteUpsert, store.BatchWriteUpsertByKey,
	} {
		advertised, ok := Ops[string(kind)].OutputSchema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s advertises no output properties", kind)
		}
		want := make([]string, 0, len(advertised)+1)
		for name := range advertised {
			if name == "replayed" {
				continue
			}
			want = append(want, name)
		}
		want = append(want, "kind")
		sort.Strings(want)

		got := names(batchWriteResult(t, kind))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("batch's %s result advertises %v, want the advertised %s output minus replayed, plus kind: %v", kind, got, kind, want)
		}
		gotRequired := requiredOf(t, batchResultEntry(t, kind))
		wantRequired := advertisedRequired(t, kind, false, func(name string) bool { return name == "replayed" })
		if !reflect.DeepEqual(gotRequired, wantRequired) {
			t.Fatalf("batch's %s result requires %v, want the advertised required list minus replayed, plus kind: %v", kind, gotRequired, wantRequired)
		}
	}
}

func TestBatchResultSchemasReadTheOverriddenOutputSchemas(t *testing.T) {
	for _, kind := range []store.BatchWriteKind{
		store.BatchWriteInsert, store.BatchWriteUpdate, store.BatchWriteDelete,
		store.BatchWriteUpsert, store.BatchWriteUpsertByKey,
	} {
		def := Ops[string(kind)]
		overridden, isOverridden := outputSchemas[string(kind)]
		if !isOverridden {
			t.Fatalf("%s is not in outputSchemas, so this test no longer proves the derivation reads the advertised schema", kind)
		}
		if !reflect.DeepEqual(def.OutputSchema, overridden) {
			t.Fatalf("%s advertises something other than its outputSchemas entry, so init order is wrong", kind)
		}
	}
}

func TestBatchDropsTheResultFieldsTheKindDoesNotReturn(t *testing.T) {
	insert := batchWriteResult(t, store.BatchWriteInsert)
	if _, ok := insert["updated"]; ok {
		t.Fatalf("an insert result advertises updated, so the per-kind derivation is not per kind: %v", names(insert))
	}
	del := batchWriteResult(t, store.BatchWriteDelete)
	if _, ok := del["inserted"]; ok {
		t.Fatalf("a delete result advertises inserted: %v", names(del))
	}
	if _, ok := del["matched"]; !ok {
		t.Fatalf("a delete result must carry matched, which is the single op's field too: %v", names(del))
	}
	upd := batchWriteResult(t, store.BatchWriteUpdate)
	if _, ok := upd["updated"]; !ok {
		t.Fatalf("an update result must carry updated: %v", names(upd))
	}
	if _, ok := upd["replayed"]; ok {
		t.Fatalf("a per-write result must not carry replayed; the batch's own result carries it once")
	}
}
