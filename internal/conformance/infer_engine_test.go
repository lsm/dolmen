package conformance

import "testing"

func TestAnInferredSchemaIsOneTheRunningEngineAccepts(t *testing.T) {
	h := newHarness(t)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "inf"})
	samples := []map[string]any{{"user": "ada", "any": 1, "pivot": true, "order": "first"}}
	for transport, inf := range map[string]map[string]any{
		"http": h.mustHTTP("infer_schema", map[string]any{"samples": samples}),
		"mcp":  h.mustMCP("infer_schema", map[string]any{"samples": samples}),
	} {
		h.mustHTTP("create_table", map[string]any{"namespace": "inf", "table": "from_" + transport, "fields": inf["fields"]})
	}
}
