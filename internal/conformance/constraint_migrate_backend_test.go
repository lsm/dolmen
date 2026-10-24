package conformance

import (
	"errors"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestConstraintMigrationBackendIsEnforcedOnHTTPAndMCP(t *testing.T) {
	if testEngine(t) == store.EngineLakehouse {
		t.Skip("lakehouse activation is a later slice")
	}
	h := newHarness(t)
	h.seedTable("app", "notes", []map[string]any{{"name": "slug", "type": "string"}})
	args := map[string]any{"namespace": "app", "table": "notes", "records": []map[string]any{{"slug": "original"}}}
	h.mustHTTP("insert", args)
	add := []schema.Change{{Op: schema.OpAddConstraint, Constraint: &schema.Constraint{Name: "slug_key", Kind: schema.ConstraintUnique, Fields: []string{"slug"}}}}
	plan, err := h.st.PlanMigration(t.Context(), "app", "notes", add, store.Embedder{}, store.Incarnation{}, nil, store.Incarnation{})
	if err != nil || !plan.DryRun {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	h.mustHTTP("insert", args)
	if _, err := h.st.Migrate(t.Context(), "app", "notes", add, store.Embedder{}, store.Incarnation{}); !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("migration after rows changed: %v", err)
	}
	h.mustHTTP("delete", map[string]any{"namespace": "app", "table": "notes", "filter": "id = 2"})
	if _, err := h.st.Migrate(t.Context(), "app", "notes", add, store.Embedder{}, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	_, body := h.httpCall("insert", args)
	httpErr := envelopeOf(t, body)
	message, _ := httpErr["message"].(string)
	if httpErr["code"] != "conflict" || !strings.Contains(message, "slug_key") || strings.Contains(message, "original") {
		t.Fatalf("HTTP conflict: %v", body)
	}
	mcpErr := h.mcpCall("insert", args).toolError()
	if mcpErr == nil || mcpErr["code"] != "conflict" || mcpErr["message"] != message {
		t.Fatalf("MCP conflict: %v", mcpErr)
	}
	if _, err := h.st.Migrate(t.Context(), "app", "notes", []schema.Change{{Op: schema.OpDropConstraint, Name: "slug_key"}}, store.Embedder{}, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	h.mustHTTP("insert", args)
}
