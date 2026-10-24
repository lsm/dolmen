package conformance

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/lsm/dolmen"
)

func wireClaim(expected any) map[string]any {
	return map[string]any{"namespace": "acme", "writes": []any{
		map[string]any{"kind": "update", "table": "jobs", "filter": "title = ? AND version = ?", "args": []any{"job", 7}, "set": map[string]any{"version": 8}, "expected_rows": expected},
		map[string]any{"kind": "insert", "table": "events", "records": []any{map[string]any{"job": "job"}}},
	}}
}

func wireError(t *testing.T, h *harness, transport string, body map[string]any) (string, string) {
	t.Helper()
	return wireErrorOp(t, h, transport, "batch", body)
}

func wireErrorOp(t *testing.T, h *harness, transport, op string, body map[string]any) (string, string) {
	t.Helper()
	if transport == "http" {
		status, out := h.httpCall(op, body)
		env, _ := out["error"].(map[string]any)
		code, _ := env["code"].(string)
		msg, _ := env["message"].(string)
		return fmt.Sprintf("%d %s", status, code), msg
	}
	result := h.mcpCall(op, body)
	if !result.isError() {
		return "ok", ""
	}
	env := result.toolError()
	code, _ := env["code"].(string)
	msg, _ := env["message"].(string)
	return code, msg
}

func wireOK(t *testing.T, h *harness, transport string, body map[string]any) map[string]any {
	t.Helper()
	if transport == "http" {
		return h.mustHTTP("batch", body)
	}
	result := h.mcpCall("batch", body)
	if result.isError() {
		t.Fatalf("MCP batch failed: %+v", result.toolError())
	}
	return result.structured()
}

func TestBatchExpectedRowsOnTheWire(t *testing.T) {
	for _, transport := range []string{"http", "mcp"} {
		t.Run(transport, func(t *testing.T) {
			h := newHarness(t)
			conditionJobs(t, h)
			out := wireOK(t, h, transport, wireClaim(1))
			results := out["results"].([]any)
			if len(results) != 2 || results[0].(map[string]any)["updated"] != float64(1) {
				t.Fatalf("claim results: %+v", out)
			}
			want := map[string]string{"http": "400 conflict", "mcp": "conflict"}[transport]
			code, msg := wireError(t, h, transport, wireClaim(1))
			if code != want || !strings.Contains(msg, "writes[0]: expected_rows mismatch") {
				t.Fatalf("stale claim: %s %q, want %s naming writes[0]", code, msg, want)
			}
			if conditionCount(t, h, "events", "1=1") != 1 || conditionCount(t, h, "jobs", "version = 8") != 1 {
				t.Fatal("stale claim left a write behind")
			}
			zero := map[string]any{"namespace": "acme", "writes": []any{
				map[string]any{"kind": "delete", "table": "events", "filter": "job = ?", "args": []any{"other"}, "expected_rows": 0},
			}}
			if r := wireOK(t, h, transport, zero)["results"].([]any)[0].(map[string]any); r["matched"] != float64(0) {
				t.Fatalf("zero assertion: %+v", r)
			}
		})
	}
}

func TestBatchExpectedRowsWireRejectsBadValues(t *testing.T) {
	bad := map[string]any{"null": nil, "negative": -1, "fractional": 1.5, "string": "1", "boolean": true, "over budget": 1001, "huge": 1e30}
	for _, transport := range []string{"http", "mcp"} {
		h := newHarness(t)
		conditionJobs(t, h)
		want := map[string]string{"http": "400 invalid_request", "mcp": "invalid_request"}[transport]
		for name, v := range bad {
			code, msg := wireError(t, h, transport, wireClaim(v))
			if code != want || !strings.Contains(msg, "writes[0]") || !strings.Contains(msg, "expected_rows") {
				t.Fatalf("%s %s: %s %q, want %s naming writes[0] and expected_rows", transport, name, code, msg, want)
			}
		}
		for _, kind := range []string{"insert", "upsert", "upsert_by_key"} {
			body := map[string]any{"namespace": "acme", "writes": []any{map[string]any{"kind": kind, "table": "events", "records": []any{map[string]any{"job": "x"}}, "on": []any{"job"}, "expected_rows": 1}}}
			if kind == "insert" || kind == "upsert" {
				delete(body["writes"].([]any)[0].(map[string]any), "on")
			}
			code, msg := wireError(t, h, transport, body)
			if code != want || !strings.Contains(msg, "expected_rows") {
				t.Fatalf("%s %s write: %s %q, want %s naming expected_rows", transport, kind, code, msg, want)
			}
		}
		for _, op := range []string{"update", "delete"} {
			body := map[string]any{"namespace": "acme", "table": "jobs", "filter": "1=1", "expected_rows": 1}
			if op == "update" {
				body["set"] = map[string]any{"version": 9}
			}
			if code, _ := wireErrorOp(t, h, transport, op, body); !strings.HasPrefix(code, map[string]string{"http": "400", "mcp": "invalid_request"}[transport]) {
				t.Fatalf("%s single %s took expected_rows: %s", transport, op, code)
			}
		}
		if conditionCount(t, h, "events", "1=1") != 0 || conditionCount(t, h, "jobs", "version = 7") != 1 {
			t.Fatalf("%s: a rejected request wrote rows", transport)
		}
	}
}

func TestBatchExpectedRowsGoFacade(t *testing.T) {
	st, _ := embeddedStore(t)
	ctx := t.Context()
	if _, err := st.CreateTable(ctx, "acme", "jobs", []dolmen.Field{{Name: "title"}, {Name: "version", Type: dolmen.Number}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Insert(ctx, "acme", "jobs", []map[string]any{{"title": "job", "version": 7}}, dolmen.InsertOptions{}); err != nil {
		t.Fatal(err)
	}
	one, zero, negative := int64(1), int64(0), int64(-1)
	claim := func(n *int64) []dolmen.BatchWrite {
		return []dolmen.BatchWrite{{Kind: dolmen.BatchUpdate, Table: "jobs", Filter: "version = ?", Args: []any{7}, Set: map[string]any{"version": 8}, ExpectedRows: n}}
	}
	if r, err := st.Batch(ctx, "acme", claim(&one), dolmen.BatchOptions{}); err != nil || r.Results[0].Updated != 1 {
		t.Fatalf("claim: %+v %v", r, err)
	}
	if _, err := st.Batch(ctx, "acme", claim(&one), dolmen.BatchOptions{}); !errors.Is(err, dolmen.ErrConflict) || !strings.Contains(err.Error(), "writes[0]: expected_rows mismatch") {
		t.Fatalf("stale claim: %v, want ErrConflict", err)
	}
	if _, err := st.Batch(ctx, "acme", claim(&zero), dolmen.BatchOptions{}); err != nil {
		t.Fatalf("zero assertion on a missed filter: %v", err)
	}
	if _, err := st.Batch(ctx, "acme", claim(&negative), dolmen.BatchOptions{}); !errors.Is(err, dolmen.ErrInvalidRequest) {
		t.Fatalf("negative expectation: %v, want ErrInvalidRequest", err)
	}
	ins := []dolmen.BatchWrite{{Kind: dolmen.BatchInsert, Table: "jobs", Records: []map[string]any{{"title": "x"}}, ExpectedRows: &one}}
	if _, err := st.Batch(ctx, "acme", ins, dolmen.BatchOptions{}); !errors.Is(err, dolmen.ErrInvalidRequest) {
		t.Fatalf("expectation on insert: %v, want ErrInvalidRequest", err)
	}
}
