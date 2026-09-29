package api

import (
	"reflect"
	"sort"
	"testing"

	"github.com/lsm/dolmen/internal/store"
)

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
			if name == "namespace" || name == "idempotency_key" || name == "dry_run" {
				continue
			}
			want = append(want, name)
		}
		want = append(want, "kind")
		sort.Strings(want)

		got := names(batchWriteInput(t, kind))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("batch's %s write advertises %v, want the %s input minus namespace, idempotency_key and dry_run, plus kind: %v", kind, got, kind, want)
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
