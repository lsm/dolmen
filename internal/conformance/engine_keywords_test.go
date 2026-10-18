package conformance

import (
	"net/http"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/store"
)

func TestNamesAnEngineReservesAreRefusedOnThatEngine(t *testing.T) {
	h := newHarness(t)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "kw"})
	refused := map[string]bool{}
	switch testEngine(t) {
	case store.EnginePostgres:
		refused["user"], refused["any"] = true, true
	case store.EngineLakehouse:
		refused["map"], refused["any"] = true, true
	}
	for i, name := range []string{"user", "any", "map"} {
		table := []string{"t_user", "t_any", "t_map"}[i]
		code, out := h.httpCall("create_table", map[string]any{"namespace": "kw", "table": table, "fields": []map[string]any{{"name": name, "type": "string"}}})
		if !refused[name] {
			if code != http.StatusOK {
				t.Fatalf("field %q must be accepted where the engine does not reserve it: %d %v", name, code, out)
			}
			continue
		}
		if msg := errMessage(out); code != http.StatusBadRequest || !strings.Contains(msg, "reserved keyword") || !strings.Contains(msg, "my_"+name) {
			t.Fatalf("field %q on an engine that reserves it: %d %v, want a refusal suggesting my_%s", name, code, out, name)
		}
		h.mustHTTP("create_table", map[string]any{"namespace": "kw", "table": table, "fields": []map[string]any{{"name": "title", "type": "string"}}})
		for _, ch := range []map[string]any{{"op": "add_field", "field": map[string]any{"name": name, "type": "string"}}, {"op": "rename_field", "from": "title", "to": name}} {
			if code, out := h.httpCall("migrate", map[string]any{"namespace": "kw", "table": table, "expected_version": 1, "changes": []any{ch}}); code != http.StatusBadRequest || !strings.Contains(errMessage(out), "reserved keyword") {
				t.Fatalf("migrate %v to %q: %d %v, want the reserved-keyword refusal", ch["op"], name, code, out)
			}
		}
	}
}
