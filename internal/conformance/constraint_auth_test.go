package conformance

import (
	"net/http"
	"strings"
	"testing"
)

func constraintAuthTables(t *testing.T) *harness {
	t.Helper()
	h := newHarnessMode(t, authGateway)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	h.mustHTTP("create_table", map[string]any{"namespace": "acme", "table": "projects", "accept_references": true,
		"fields": []map[string]any{{"name": "slug", "type": "string"}}})
	h.mustHTTP("create_table", map[string]any{"namespace": "acme", "table": "tasks",
		"fields": []map[string]any{{"name": "project_id", "type": "number"}, {"name": "slug", "type": "string"}},
		"constraints": []map[string]any{
			{"name": "project_ref", "kind": "foreign_key", "field": "project_id", "references": map[string]any{"table": "projects", "field": "id"}},
			{"name": "slug_key", "kind": "unique", "fields": []string{"slug"}},
		}})
	h.mustHTTP("create_table", map[string]any{"namespace": "acme", "table": "orphans", "accept_references": true,
		"fields": []map[string]any{{"name": "slug", "type": "string"}}})
	h.mustHTTP("insert", map[string]any{"namespace": "acme", "table": "projects", "records": []map[string]any{{"slug": "p"}}})
	h.mustHTTP("insert", map[string]any{"namespace": "acme", "table": "orphans", "records": []map[string]any{{"slug": "o"}}})
	return h
}

func TestConstrainedWritesNeedReadOnEveryTableTheirChecksConsult(t *testing.T) {
	h := constraintAuthTables(t)
	insert := `{"namespace":"acme","table":"tasks","records":[{"project_id":1,"slug":"a"}]}`
	grantTo(t, h, "principal", "alice", "acme", "tasks", "create")
	if res, out := h.asIdentity(t, "alice", "", "insert", insert); res.StatusCode != http.StatusForbidden || errCode(out) != "forbidden" {
		t.Fatalf("create alone on a constrained table: %d %v", res.StatusCode, out)
	}
	grantTo(t, h, "principal", "alice", "acme", "tasks", "read")
	if res, out := h.asIdentity(t, "alice", "", "insert", insert); res.StatusCode != http.StatusForbidden {
		t.Fatalf("without read on the parent the foreign key consults: %d %v", res.StatusCode, out)
	}
	grantTo(t, h, "principal", "alice", "acme", "projects", "read")
	if res, out := h.asIdentity(t, "alice", "", "insert", insert); res.StatusCode != http.StatusOK {
		t.Fatalf("with read on both endpoints: %d %v", res.StatusCode, out)
	}
	batch := `{"namespace":"acme","writes":[{"kind":"insert","table":"tasks","records":[{"project_id":1,"slug":"b"}]}]}`
	grantTo(t, h, "principal", "bob", "acme", "", "admin", "create")
	if res, out := h.asIdentity(t, "bob", "", "batch", batch); res.StatusCode != http.StatusForbidden || !strings.Contains(errMessage(out), "read verb on each") {
		t.Fatalf("a batched write takes the same gate: %d %v", res.StatusCode, out)
	}
	grantTo(t, h, "principal", "bob", "acme", "", "read")
	if res, out := h.asIdentity(t, "bob", "", "batch", batch); res.StatusCode != http.StatusOK {
		t.Fatalf("a batched write with read on both endpoints: %d %v", res.StatusCode, out)
	}
}

func TestDeletingFromATableThatAcceptsReferencesNeedsNamespaceRead(t *testing.T) {
	h := constraintAuthTables(t)
	for _, table := range []string{"projects", "orphans"} {
		grantTo(t, h, "principal", "dave", "acme", table, "delete", "read")
	}
	var outcomes []string
	for _, table := range []string{"projects", "orphans"} {
		res, out := h.asIdentity(t, "dave", "", "delete", `{"namespace":"acme","table":"`+table+`","filter":"id = 1"}`)
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("delete on %s without namespace read: %d %v", table, res.StatusCode, out)
		}
		outcomes = append(outcomes, errMessage(out))
	}
	if outcomes[0] != outcomes[1] {
		t.Fatalf("the refusal must not depend on whether the table is referenced: %q vs %q", outcomes[0], outcomes[1])
	}
	if res, out := h.asIdentity(t, "dave", "", "drop_table", `{"namespace":"acme","table":"orphans","confirm":"orphans"}`); res.StatusCode != http.StatusForbidden {
		t.Fatalf("drop_table without namespace read: %d %v", res.StatusCode, out)
	}
	grantTo(t, h, "principal", "dave", "acme", "", "read")
	if res, out := h.asIdentity(t, "dave", "", "delete", `{"namespace":"acme","table":"orphans","filter":"id = 1"}`); res.StatusCode != http.StatusOK {
		t.Fatalf("with namespace read: %d %v", res.StatusCode, out)
	}
}

func TestDeclaringReferencesNeedsNamespaceRead(t *testing.T) {
	h := constraintAuthTables(t)
	grantTo(t, h, "principal", "erin", "acme", "", "schema")
	create := `{"namespace":"acme","table":"teams","accept_references":true,"fields":[{"name":"slug","type":"string"}]}`
	if res, out := h.asIdentity(t, "erin", "", "create_table", create); res.StatusCode != http.StatusForbidden {
		t.Fatalf("accept_references with schema only: %d %v", res.StatusCode, out)
	}
	migrate := `{"namespace":"acme","table":"tasks","changes":[{"op":"set_accept_references","value":true}]}`
	if res, out := h.asIdentity(t, "erin", "", "migrate", migrate); res.StatusCode != http.StatusForbidden {
		t.Fatalf("set_accept_references with schema only: %d %v", res.StatusCode, out)
	}
	grantTo(t, h, "principal", "erin", "acme", "", "read")
	if res, out := h.asIdentity(t, "erin", "", "create_table", create); res.StatusCode != http.StatusOK {
		t.Fatalf("with schema and namespace read: %d %v", res.StatusCode, out)
	}
}
