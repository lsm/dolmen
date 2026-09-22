package conformance

import (
	"net/http"
	"strings"
	"testing"
)

func seedMigratableTable(t *testing.T) *harness {
	t.Helper()
	h := newHarnessMode(t, authAdminKey)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	h.mustHTTP("create_table", map[string]any{
		"namespace": "acme", "table": "notes",
		"fields": []map[string]any{{"name": "body", "type": "text"}},
	})
	return h
}

func planToken(t *testing.T, h *harness) string {
	t.Helper()
	data := h.mustHTTP("migrate", map[string]any{
		"namespace": "acme", "table": "notes", "dry_run": true,
		"changes": []map[string]any{{"op": "add_field", "field": map[string]any{"name": "tag", "type": "string"}}},
	})
	plan, _ := data["plan"].(map[string]any)
	token, _ := plan["expected_incarnation"].(string)
	if token == "" {
		t.Fatalf("the dry run minted no expected_incarnation: %v", data)
	}
	return token
}

func TestAPlanBindsToTheTableItSaw(t *testing.T) {
	h := seedMigratableTable(t)
	token := planToken(t, h)

	status, out := h.httpCall("migrate", map[string]any{
		"namespace": "acme", "table": "notes", "expected_incarnation": token,
		"changes": []map[string]any{{"op": "add_field", "field": map[string]any{"name": "tag", "type": "string"}}},
	})
	if status != http.StatusOK {
		t.Fatalf("the table has not moved since the plan, so it applies: %d %v", status, out)
	}
}

func TestAPlanCannotApplyToASameNamedSuccessor(t *testing.T) {
	h := seedMigratableTable(t)
	token := planToken(t, h)

	h.mustHTTP("drop_table", map[string]any{"namespace": "acme", "table": "notes", "confirm": "notes"})
	h.mustHTTP("create_table", map[string]any{
		"namespace": "acme", "table": "notes",
		"fields": []map[string]any{{"name": "body", "type": "text"}},
	})

	status, out := h.httpCall("migrate", map[string]any{
		"namespace": "acme", "table": "notes", "expected_incarnation": token,
		"changes": []map[string]any{{"op": "add_field", "field": map[string]any{"name": "tag", "type": "string"}}},
	})
	if status == http.StatusOK {
		t.Fatalf("a plan made against a dropped table applied to its same-named successor, which is the race the token closes: %v", out)
	}
	errObj, _ := out["error"].(map[string]any)
	if code, _ := errObj["code"].(string); code != "not_found" {
		t.Fatalf("the successor is a different table, so the refusal is not_found: %v", out)
	}

	if status, out := h.httpCall("migrate", map[string]any{
		"namespace": "acme", "table": "notes",
		"changes": []map[string]any{{"op": "add_field", "field": map[string]any{"name": "tag", "type": "string"}}},
	}); status != http.StatusOK {
		t.Fatalf("the same migration with no precondition applies to the successor, which is what the token is protecting against: %d %v", status, out)
	}
}

func TestAVersionOnlyPreconditionIsRefusedUnderAuth(t *testing.T) {
	h := seedMigratableTable(t)
	status, out := h.httpCall("migrate", map[string]any{
		"namespace": "acme", "table": "notes", "expected_version": 1,
		"changes": []map[string]any{{"op": "add_field", "field": map[string]any{"name": "tag", "type": "string"}}},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("version 1 cannot tell a table from a same-named predecessor, so a version-only precondition must be refused under auth: %d %v", status, out)
	}
	errObj, _ := out["error"].(map[string]any)
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "expected_incarnation") {
		t.Fatalf("the refusal does not name the token to use instead: %v", out)
	}
}

func TestAVersionOnlyPreconditionStillWorksWithAuthOff(t *testing.T) {
	h := newHarnessMode(t, authOff)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	h.mustHTTP("create_table", map[string]any{
		"namespace": "acme", "table": "notes",
		"fields": []map[string]any{{"name": "body", "type": "text"}},
	})
	status, out := h.httpCall("migrate", map[string]any{
		"namespace": "acme", "table": "notes", "expected_version": 1,
		"changes": []map[string]any{{"op": "add_field", "field": map[string]any{"name": "tag", "type": "string"}}},
	})
	if status != http.StatusOK {
		t.Fatalf("expected_version alone is the auth-off compatibility path and must keep working: %d %v", status, out)
	}
}
