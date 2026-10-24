package conformance

import (
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func relationSetup(t *testing.T, h *harness) {
	t.Helper()
	if testEngine(t) != store.EngineSQLite {
		t.Skip("relation tables are SQLite-only until the other engines keep the declaration")
	}
	h.ensureNS("acme")
	ctx := t.Context()
	if _, err := h.st.CreateTable(ctx, "acme", "nodes", []schema.Field{{Name: "label", Type: schema.String}}, store.TableOpts{AcceptReferences: true}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	fields := []schema.Field{{Name: "src", Type: schema.Number, Required: true}, {Name: "dst", Type: schema.Number, Required: true}, {Name: "kind", Type: schema.String, Required: true}, {Name: "weight", Type: schema.Number}}
	opts := store.TableOpts{
		Constraints: []schema.Constraint{
			{Name: "src_ref", Kind: schema.ConstraintForeignKey, Field: "src", References: &schema.ConstraintReference{Table: "nodes", Field: "id"}},
			{Name: "dst_ref", Kind: schema.ConstraintForeignKey, Field: "dst", References: &schema.ConstraintReference{Table: "nodes", Field: "id"}},
		},
		Relation: &schema.Relation{Source: "src", Target: "dst", Type: "kind"},
	}
	if _, err := h.st.CreateTable(ctx, "acme", "links", fields, opts, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	h.mustHTTP("insert", map[string]any{"namespace": "acme", "table": "nodes", "records": []any{map[string]any{"label": "n1"}, map[string]any{"label": "n2"}}})
	h.seedTable("acme", "plain", []map[string]any{{"name": "label"}})
}

func relateCall(t *testing.T, h *harness, transport string, body map[string]any) (map[string]any, string, string) {
	t.Helper()
	if transport == "http" {
		_, out := h.httpCall("relate", body)
		if out["ok"] == true {
			return out["data"].(map[string]any), "", ""
		}
		env, _ := out["error"].(map[string]any)
		code, _ := env["code"].(string)
		msg, _ := env["message"].(string)
		return nil, code, msg
	}
	result := h.mcpCall("relate", body)
	if !result.isError() {
		return result.structured(), "", ""
	}
	env := result.toolError()
	code, _ := env["code"].(string)
	msg, _ := env["message"].(string)
	return nil, code, msg
}

func TestRelateInsertsOneIdempotentEdge(t *testing.T) {
	for _, transport := range []string{"http", "mcp"} {
		t.Run(transport, func(t *testing.T) {
			h := newHarness(t)
			relationSetup(t, h)
			edge := map[string]any{"namespace": "acme", "table": "links", "source": 1, "target": 2, "type": "k", "properties": map[string]any{"weight": 0.5}, "idempotency_key": "e1"}
			out, code, msg := relateCall(t, h, transport, edge)
			if out == nil || out["id"] != float64(1) || out["replayed"] != false {
				t.Fatalf("relate: %+v %s %s", out, code, msg)
			}
			again, _, _ := relateCall(t, h, transport, edge)
			if again == nil || again["id"] != float64(1) || again["replayed"] != true {
				t.Fatalf("replay: %+v", again)
			}
			rows := h.mustHTTP("query", map[string]any{"namespace": "acme", "sql": "SELECT src, dst, kind, weight FROM links"})["rows"].([]any)
			if len(rows) != 1 {
				t.Fatalf("relate stored %d rows: %+v", len(rows), rows)
			}
			if r := rows[0].(map[string]any); r["src"] != float64(1) || r["dst"] != float64(2) || r["kind"] != "k" || r["weight"] != 0.5 {
				t.Fatalf("stored edge: %+v", r)
			}
			changed := map[string]any{"namespace": "acme", "table": "links", "source": 1, "target": 2, "type": "other", "idempotency_key": "e1"}
			if _, code, _ := relateCall(t, h, transport, changed); code != "conflict" {
				t.Fatalf("a reused key for another edge: %s, want conflict", code)
			}
			for label, c := range map[string]struct {
				body map[string]any
				code string
				want string
			}{
				"missing endpoint":       {map[string]any{"namespace": "acme", "table": "links", "source": 1, "target": 9, "type": "k"}, "conflict", "dst_ref"},
				"not a relation":         {map[string]any{"namespace": "acme", "table": "plain", "source": 1, "target": 2, "type": "k"}, "invalid_request", "insert"},
				"role in properties":     {map[string]any{"namespace": "acme", "table": "links", "source": 1, "target": 2, "type": "k", "properties": map[string]any{"src": 2}}, "invalid_request", "source"},
				"unknown property":       {map[string]any{"namespace": "acme", "table": "links", "source": 1, "target": 2, "type": "k", "properties": map[string]any{"color": "red"}}, "invalid_request", "color"},
				"missing type":           {map[string]any{"namespace": "acme", "table": "links", "source": 1, "target": 2}, "invalid_request", "type"},
				"fractional source":      {map[string]any{"namespace": "acme", "table": "links", "source": 1.5, "target": 2, "type": "k"}, "invalid_request", "source"},
				"missing relation table": {map[string]any{"namespace": "acme", "table": "absent", "source": 1, "target": 2, "type": "k"}, "not_found", "absent"},
			} {
				if out, code, msg := relateCall(t, h, transport, c.body); out != nil || code != c.code || !strings.Contains(msg, c.want) {
					t.Fatalf("%s: %+v %s %q, want %s naming %q", label, out, code, msg, c.code, c.want)
				}
			}
			if n := h.mustHTTP("query", map[string]any{"namespace": "acme", "sql": "SELECT count(*) AS n FROM links"})["rows"].([]any)[0].(map[string]any)["n"]; n != float64(1) {
				t.Fatalf("a refused relate wrote an edge: %v rows", n)
			}
		})
	}
}
