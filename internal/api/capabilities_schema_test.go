package api

import (
	"encoding/json"
	"testing"

	"github.com/lsm/dolmen/internal/store"
)

func TestTheCapabilitiesSchemaDeclaresEveryFieldItReturns(t *testing.T) {
	raw, err := json.Marshal(store.EngineCapabilities{})
	if err != nil {
		t.Fatal(err)
	}
	var returned map[string]any
	if err := json.Unmarshal(raw, &returned); err != nil {
		t.Fatal(err)
	}

	schema, ok := outputSchemas["capabilities"]
	if !ok {
		t.Fatal("capabilities has no published output schema")
	}
	props, _ := schema["properties"].(map[string]any)
	required := map[string]bool{}
	if list, ok := schema["required"].([]string); ok {
		for _, name := range list {
			required[name] = true
		}
	}
	for field := range returned {
		if _, declared := props[field]; !declared {
			t.Fatalf("capabilities returns %q but the published schema does not declare it, and the schema forbids additional properties — a client validating the response would reject it", field)
		}
		if !required[field] {
			t.Fatalf("capabilities always returns %q, so the schema must require it: discovery is only portable if a client can rely on the field being there", field)
		}
	}

	def, ok := Ops["capabilities"]
	if !ok {
		t.Fatal("no capabilities operation")
	}
	inline, _ := def.OutputSchema["properties"].(map[string]any)
	for field := range returned {
		if _, declared := inline[field]; !declared {
			t.Fatalf("the operation's own schema omits %q; MCP tools/list reads this one", field)
		}
	}
}
