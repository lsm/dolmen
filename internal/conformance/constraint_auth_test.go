package conformance

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
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
	h.mustHTTP("insert", map[string]any{"namespace": "acme", "table": "tasks", "records": []map[string]any{{"project_id": 1, "slug": "linked"}}})
	for _, table := range []string{"projects", "orphans"} {
		grantTo(t, h, "principal", "dave", "acme", table, "delete", "read", "schema", "admin")
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
	if res, out := h.asIdentity(t, "dave", "", "drop_table", `{"namespace":"acme","table":"orphans","confirm":"orphans"}`); res.StatusCode != http.StatusForbidden || !strings.Contains(errMessage(out), "read on the whole namespace") {
		t.Fatalf("drop_table without namespace read: %d %v", res.StatusCode, out)
	}
	grantTo(t, h, "principal", "dave", "acme", "tasks", "read")
	if res, out := h.asIdentity(t, "dave", "", "delete", `{"namespace":"acme","table":"projects","filter":"id = 1"}`); res.StatusCode != http.StatusForbidden || errMessage(out) != outcomes[0] {
		t.Fatalf("readable and hidden incoming tables must have the same gate: %d %v", res.StatusCode, out)
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
	grantTo(t, h, "principal", "erin", "acme", "tasks", "read")
	if res, out := h.asIdentity(t, "erin", "", "migrate", migrate); res.StatusCode != http.StatusForbidden || !strings.Contains(errMessage(out), "read on the whole namespace") {
		t.Fatalf("table read is not namespace read: %d %v", res.StatusCode, out)
	}
	grantTo(t, h, "principal", "erin", "acme", "", "read")
	if res, out := h.asIdentity(t, "erin", "", "create_table", create); res.StatusCode != http.StatusOK {
		t.Fatalf("with schema and namespace read: %d %v", res.StatusCode, out)
	}
}

func TestOwnConstraintChecksRequireAllDomainsForTableWideMutation(t *testing.T) {
	h := newHarnessMode(t, authGateway)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	h.mustHTTP("create_table", map[string]any{"namespace": "acme", "table": "parents", "row_access": "own", "accept_references": true, "fields": []map[string]any{{"name": "slug", "type": "string"}}})
	h.mustHTTP("create_table", map[string]any{"namespace": "acme", "table": "children", "row_access": "own", "fields": []map[string]any{{"name": "parent_id", "type": "number"}}, "constraints": []map[string]any{{"name": "parent_ref", "kind": "foreign_key", "field": "parent_id", "references": map[string]any{"table": "parents", "field": "id"}}}})
	grantTo(t, h, "principal", "alice", "acme", "parents", "create")
	grantTo(t, h, "principal", "alice", "acme", "children", "create", "update")
	for _, item := range []struct{ op, body string }{
		{"insert", `{"namespace":"acme","table":"parents","records":[{"slug":"p"}]}`},
		{"insert", `{"namespace":"acme","table":"children","records":[{"parent_id":1}]}`},
		{"update", `{"namespace":"acme","table":"children","filter":"id = 1","set":{"parent_id":1}}`},
	} {
		if res, out := h.asIdentity(t, "alice", "", item.op, item.body); res.StatusCode != http.StatusOK {
			t.Fatalf("standing own-row reads suffice: %s %d %v", item.op, res.StatusCode, out)
		}
	}
	grantTo(t, h, "principal", "alice", "acme", "children", "read")
	body := `{"namespace":"acme","table":"children","filter":"1 = 1","set":{"parent_id":1}}`
	if res, out := h.asIdentity(t, "alice", "", "update", body); res.StatusCode != http.StatusForbidden || errCode(out) != "forbidden" {
		t.Fatalf("table-wide mutation without all parent domains: %d %v", res.StatusCode, out)
	}
	grantTo(t, h, "principal", "alice", "acme", "parents", "read")
	if res, out := h.asIdentity(t, "alice", "", "update", body); res.StatusCode != http.StatusOK {
		t.Fatalf("table-wide mutation with all endpoint reads: %d %v", res.StatusCode, out)
	}
}

func TestImportAndRelationWritesTakeConstraintReadGates(t *testing.T) {
	h := constraintAuthTables(t)
	grantTo(t, h, "principal", "importer", "acme", "tasks", "admin", "create")
	body := `{"namespace":"acme","table":"tasks","idempotency_key":"chunk-1","records":[{"id":20,"created_at":"2026-01-01T00:00:00.000Z","project_id":1,"slug":"imported"}]}`
	if res, out := h.asIdentity(t, "importer", "", "import_records", body); res.StatusCode != http.StatusForbidden || errCode(out) != "forbidden" {
		t.Fatalf("import without endpoint reads: %d %v", res.StatusCode, out)
	}
	grantTo(t, h, "principal", "importer", "acme", "tasks", "read")
	grantTo(t, h, "principal", "importer", "acme", "projects", "read")
	if res, out := h.asIdentity(t, "importer", "", "import_records", body); res.StatusCode != http.StatusOK {
		t.Fatalf("import with endpoint reads: %d %v", res.StatusCode, out)
	}
	if testEngine(t) != "sqlite" {
		return
	}
	h.mustHTTP("create_namespace", map[string]any{"namespace": "app"})
	h.mustHTTP("create_table", map[string]any{"namespace": "app", "table": "nodes", "accept_references": true, "fields": []map[string]any{{"name": "label", "type": "string"}}})
	h.mustHTTP("insert", map[string]any{"namespace": "app", "table": "nodes", "records": []map[string]any{{"label": "n"}}})
	h.mustHTTP("create_table", relationTableBody("links"))
	grantTo(t, h, "principal", "linker", "app", "links", "create")
	body = `{"namespace":"app","table":"links","source":1,"target":1,"type":"k"}`
	if res, out := h.asIdentity(t, "linker", "", "relate", body); res.StatusCode != http.StatusForbidden || errCode(out) != "forbidden" {
		t.Fatalf("relate without endpoint reads: %d %v", res.StatusCode, out)
	}
	grantTo(t, h, "principal", "linker", "app", "links", "read")
	grantTo(t, h, "principal", "linker", "app", "nodes", "read")
	if res, out := h.asIdentity(t, "linker", "", "relate", body); res.StatusCode != http.StatusOK {
		t.Fatalf("relate with endpoint reads: %d %v", res.StatusCode, out)
	}
}

func TestOptedInRowAccessChangeDoesNotRevealIncomingReferences(t *testing.T) {
	var outcomes []string
	for _, reach := range []string{"none", "readable", "hidden"} {
		h := newHarnessMode(t, authGateway)
		h.mustHTTP("create_namespace", map[string]any{"namespace": "policy"})
		h.mustHTTP("create_table", map[string]any{"namespace": "policy", "table": "parent", "accept_references": true, "fields": []map[string]any{{"name": "slug", "type": "string"}}})
		if reach != "none" {
			h.mustHTTP("create_table", map[string]any{"namespace": "policy", "table": "child", "fields": []map[string]any{{"name": "ref", "type": "number"}}, "constraints": []map[string]any{{"name": "parent_ref", "kind": "foreign_key", "field": "ref", "references": map[string]any{"table": "parent", "field": "id"}}}})
		}
		grantTo(t, h, "principal", "editor", "policy", "parent", "schema", "admin", "read")
		if reach == "readable" {
			grantTo(t, h, "principal", "editor", "policy", "child", "read")
		}
		for _, value := range []string{"true", "false"} {
			res, out := h.asIdentity(t, "editor", "", "migrate", `{"namespace":"policy","table":"parent","changes":[{"op":"set_row_access","value":`+value+`}]}`)
			if res.StatusCode != http.StatusForbidden || errCode(out) != "forbidden" {
				t.Errorf("%s value=%s: without namespace read: %d %v", reach, value, res.StatusCode, out)
			}
			outcomes = append(outcomes, errMessage(out))
		}
		grantTo(t, h, "principal", "editor", "policy", "", "read")
		res, out := h.asIdentity(t, "editor", "", "migrate", `{"namespace":"policy","table":"parent","changes":[{"op":"set_row_access","value":true}]}`)
		want := http.StatusBadRequest
		if reach == "none" {
			want = http.StatusOK
		}
		if res.StatusCode != want {
			t.Errorf("%s: namespace read admits the unchanged backend policy: %d %v", reach, res.StatusCode, out)
		}
	}
	for _, outcome := range outcomes {
		if outcome != outcomes[0] {
			t.Fatalf("incoming reference visibility changed the gate: %v", outcomes)
		}
	}
	h := newHarnessMode(t, authGateway)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "plain"})
	h.mustHTTP("create_table", map[string]any{"namespace": "plain", "table": "notes", "fields": []map[string]any{{"name": "slug", "type": "string"}}})
	grantTo(t, h, "principal", "editor", "plain", "notes", "schema", "admin", "read")
	if res, out := h.asIdentity(t, "editor", "", "migrate", `{"namespace":"plain","table":"notes","changes":[{"op":"set_row_access","value":true}]}`); res.StatusCode != http.StatusOK {
		t.Fatalf("ordinary table must not acquire a namespace-read gate: %d %v", res.StatusCode, out)
	}
}

type constraintMetadataFault struct {
	store.Engine
	fail atomic.Bool
}

func (f *constraintMetadataFault) TableState(ctx context.Context, ns, table string, auth []store.AuthBinding) (*schema.TableSchema, store.Incarnation, error) {
	if f.fail.Swap(false) {
		return nil, store.Incarnation{}, derr.New(derr.StorageUnavailable, "synthetic metadata unavailable")
	}
	return f.Engine.TableState(ctx, ns, table, auth)
}

func TestConstraintMetadataFailuresNeverSkipReadGates(t *testing.T) {
	for _, tc := range []struct{ op, body string }{
		{"insert", `{"namespace":"acme","table":"tasks","records":[{"project_id":1,"slug":"blocked"}]}`},
		{"migrate", `{"namespace":"acme","table":"tasks","changes":[{"op":"drop_constraint","name":"project_ref"}]}`},
		{"migrate", `{"namespace":"acme","table":"projects","changes":[{"op":"set_row_access","value":true}]}`},
		{"drop_table", `{"namespace":"acme","table":"projects","confirm":"projects"}`},
	} {
		h := constraintAuthTables(t)
		grantTo(t, h, "principal", "writer", "acme", "tasks", "create", "schema", "admin")
		grantTo(t, h, "principal", "writer", "acme", "projects", "schema", "admin", "read")
		fault := &constraintMetadataFault{Engine: h.st}
		h.st = fault
		h.restart()
		fault.fail.Store(true)
		res, out := h.asIdentity(t, "writer", "", tc.op, tc.body)
		if res.StatusCode != http.StatusServiceUnavailable || errCode(out) != "storage_unavailable" {
			t.Errorf("%s must stop when its policy cannot be read: %d %v", tc.op, res.StatusCode, out)
		}
		desc := h.mustHTTP("describe_table", map[string]any{"namespace": "acme", "table": "tasks"})["table"].(map[string]any)
		if cs, _ := desc["constraints"].([]any); len(cs) != 2 {
			t.Errorf("a policy lookup failure let a schema change through: %v", desc)
		}
		rows := h.mustHTTP("read_rows", map[string]any{"namespace": "acme", "table": "tasks"})["rows"].([]any)
		if len(rows) != 0 {
			t.Errorf("a policy lookup failure let a write through: %v", rows)
		}
	}
}
