package conformance

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/secret"
	"github.com/lsm/dolmen/internal/store"
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

func TestMaskingASecretKeepsTheEmbeddingHidden(t *testing.T) {
	h := newHarness(t)
	h.seedTable("sec", "notes", []map[string]any{
		{"name": "body", "type": "text", "vectorize": true},
		{"name": "token", "type": "secret"},
	})
	h.mustHTTP("insert", map[string]any{"namespace": "sec", "table": "notes", "records": []map[string]any{
		{"body": "pump overheating", "token": conformanceSecret},
	}})
	data := h.mustHTTP("query", map[string]any{"namespace": "sec", "sql": "SELECT * FROM notes"})
	rows, _ := data["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("SELECT * returned %d rows", len(rows))
	}
	row, _ := rows[0].(map[string]any)
	if _, ok := row["_embedding"]; ok {
		t.Fatalf("SELECT * on a table holding a secret exposed the hidden _embedding column: %v", row)
	}
	if row["token"] != secret.Mask {
		t.Fatalf("SELECT * returned token %v, want the mask", row["token"])
	}
}

func TestShapesAMaskedTableCannotTakeAreRefusedClearly(t *testing.T) {
	h := newHarness(t)
	seedQueryableSecrets(t, h)
	sqlite := testEngine(t) == store.EngineSQLite
	for _, c := range []struct {
		sql  string
		hint string
	}{
		{"SELECT rowid FROM creds", "use id"},
		{"SELECT label FROM creds NOT INDEXED", "index hint"},
		{"SELECT token FROM main.creds", "without a schema prefix"},
	} {
		status, out := h.httpCall("query", map[string]any{"namespace": "sec", "sql": c.sql})
		if status != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400: %v", c.sql, status, out)
		}
		raw, _ := json.Marshal(out)
		if strings.Contains(string(raw), "PLAINTEXT") {
			t.Fatalf("%s leaked plaintext in its refusal: %s", c.sql, raw)
		}
		if !sqlite {
			continue
		}
		errEnv, _ := out["error"].(map[string]any)
		if msg, _ := errEnv["message"].(string); !strings.Contains(msg, c.hint) {
			t.Fatalf("%s: the refusal %q must say %q", c.sql, msg, c.hint)
		}
	}

	h.seedTable("sec", "plain", []map[string]any{{"name": "note", "type": "string"}})
	h.mustHTTP("insert", map[string]any{"namespace": "sec", "table": "plain", "records": []map[string]any{{"note": "a"}, {"note": "b"}}})
	for _, sql := range []string{
		"SELECT id FROM creds ORDER BY id",
		"SELECT id AS rowid FROM creds ORDER BY id",
	} {
		h.mustHTTP("query", map[string]any{"namespace": "sec", "sql": sql})
	}
	if sqlite {
		h.mustHTTP("query", map[string]any{"namespace": "sec", "sql": "SELECT p.rowid AS r FROM creds c JOIN plain p ON p.id = c.id ORDER BY r"})
	}
}

func TestFiltersStillEvaluateAgainstTheCiphertext(t *testing.T) {
	h := newHarness(t)
	h.seedTable("sec", "creds", []map[string]any{
		{"name": "label", "type": "string", "fulltext": true},
		{"name": "token", "type": "secret"},
	})
	h.mustHTTP("insert", map[string]any{"namespace": "sec", "table": "creds", "records": []map[string]any{
		{"label": "alpha", "token": conformanceSecret},
	}})

	search := h.mustHTTP("search_fulltext", map[string]any{"namespace": "sec", "table": "creds", "query": "alpha", "filter": "token = ?", "args": []any{secret.Mask}})
	if results, _ := search["results"].([]any); len(results) != 0 {
		t.Fatalf("a search filter compared the secret with the mask and matched: %v", results)
	}
	preview := h.mustHTTP("delete", map[string]any{"namespace": "sec", "table": "creds", "filter": "token = ?", "args": []any{secret.Mask}, "dry_run": true})
	if n := int64val(t, "matched", preview["matched"]); n != 0 {
		t.Fatalf("a write filter compared the secret with the mask and matched %d rows", n)
	}
}
