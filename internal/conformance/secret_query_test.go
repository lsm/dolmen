package conformance

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/secret"
)

func seedQueryableSecrets(t *testing.T, h *harness) {
	t.Helper()
	h.seedTable("sec", "creds", []map[string]any{
		{"name": "label", "type": "string"},
		{"name": "token", "type": "secret"},
	})
	h.mustHTTP("insert", map[string]any{"namespace": "sec", "table": "creds", "records": []map[string]any{
		{"label": "alpha", "token": conformanceSecret},
		{"label": "unset"},
	}})
}

func queryCells(t *testing.T, h *harness, sql string, col string, args ...any) []any {
	t.Helper()
	body := map[string]any{"namespace": "sec", "sql": sql}
	if len(args) > 0 {
		body["args"] = args
	}
	data := h.mustHTTP("query", body)
	raw, _ := json.Marshal(data)
	if strings.Contains(string(raw), "PLAINTEXT") {
		t.Fatalf("%s returned plaintext: %s", sql, raw)
	}
	rows, _ := data["rows"].([]any)
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		row, _ := r.(map[string]any)
		out = append(out, row[col])
	}
	return out
}

func TestQueryNeverHandsOutSecretCiphertext(t *testing.T) {
	h := newHarness(t)
	seedQueryableSecrets(t, h)

	for _, c := range []struct {
		name string
		sql  string
		col  string
	}{
		{"plain column", "SELECT token FROM creds ORDER BY id", "token"},
		{"aliased column", "SELECT token AS x FROM creds ORDER BY id", "x"},
		{"aliased as another field's name", "SELECT token AS label FROM creds ORDER BY id", "label"},
		{"through a subquery", "SELECT x AS leaked FROM (SELECT token AS x FROM creds) s ORDER BY leaked", "leaked"},
		{"star", "SELECT * FROM creds ORDER BY id", "token"},
		{"qualified star", "SELECT creds.* FROM creds ORDER BY id", "token"},
		{"table-qualified column", "SELECT creds.token AS x FROM creds ORDER BY id", "x"},
		{"aliased table", "SELECT c.token AS x FROM creds c ORDER BY id", "x"},
		{"through a CTE", "WITH s AS (SELECT token AS x FROM creds) SELECT x AS leaked FROM s ORDER BY leaked", "leaked"},
	} {
		got := queryCells(t, h, c.sql, c.col)
		if len(got) != 2 {
			t.Fatalf("%s: %d rows, want 2", c.name, len(got))
		}
		masked, unset := 0, 0
		for _, v := range got {
			switch v {
			case secret.Mask:
				masked++
			case nil:
				unset++
			default:
				t.Fatalf("%s: a secret came back as %v; only the mask or null may leave the server", c.name, v)
			}
		}
		if masked != 1 || unset != 1 {
			t.Fatalf("%s: %d masked and %d null, want one of each", c.name, masked, unset)
		}
	}
}

func TestQueryCannotMeasureASecret(t *testing.T) {
	h := newHarness(t)
	seedQueryableSecrets(t, h)

	lengths := queryCells(t, h, "SELECT length(token) AS n FROM creds WHERE label = 'alpha'", "n")
	if len(lengths) != 1 {
		t.Fatalf("length query returned %d rows", len(lengths))
	}
	if got := float(t, "n", lengths[0]); int(got) != len([]rune(secret.Mask)) {
		t.Fatalf("length(token) = %v; a secret's stored length must not be measurable, only the mask's %d", got, len([]rune(secret.Mask)))
	}

	sorted := queryCells(t, h, "SELECT token AS x FROM creds ORDER BY token DESC", "x")
	if len(sorted) != 2 {
		t.Fatalf("ordering by a secret returned %d rows", len(sorted))
	}

	matched := queryCells(t, h, "SELECT label FROM creds WHERE token = ?", "label", conformanceSecret)
	if len(matched) != 0 {
		t.Fatalf("comparing a secret with a plaintext matched %v", matched)
	}
}
