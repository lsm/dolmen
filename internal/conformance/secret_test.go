package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	dolmen "github.com/lsm/dolmen"
	"github.com/lsm/dolmen/internal/secret"
	"github.com/lsm/dolmen/internal/store"
)

const conformanceSecret = "sk-conformance-PLAINTEXT-42"

func conformanceKey() []byte { return bytes.Repeat([]byte{7}, secret.KeySize) }

func conformanceKeyring(t *testing.T) *secret.Keyring {
	t.Helper()
	k, err := secret.New(conformanceKey())
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func secretTableFields() []map[string]any {
	return []map[string]any{
		{"name": "label", "type": "string", "fulltext": true},
		{"name": "token", "type": "secret"},
		{"name": "emb", "type": "vector", "dim": 3},
	}
}

func noPlaintext(t *testing.T, what string, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if strings.Contains(string(b), "PLAINTEXT") {
		t.Fatalf("%s carries plaintext: %s", what, b)
	}
}

func tokensOf(t *testing.T, rows any) map[string]any {
	t.Helper()
	out := map[string]any{}
	list, ok := rows.([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("expected rows, got %v", rows)
	}
	for _, r := range list {
		m := r.(map[string]any)
		out[m["label"].(string)] = m["token"]
	}
	return out
}

func TestSecretFieldPostgresRefused(t *testing.T) {
	if testEngine(t) != store.EnginePostgres {
		t.Skip("postgres refusal only")
	}
	h := newHarness(t)
	h.ensureNS("sec")
	status, body := h.httpCall("create_table", map[string]any{"namespace": "sec", "table": "creds", "fields": secretTableFields()})
	if status != http.StatusBadRequest {
		t.Fatalf("status %d: %v", status, body)
	}
	wantMessage(t, "postgres secret", envelopeOf(t, body)["message"].(string), `not yet supported on the postgres engine`)
}

func TestSecretFieldContract(t *testing.T) {
	sqliteOnly(t)
	h := newHarness(t)
	h.seedTable("sec", "creds", secretTableFields())
	h.mustHTTP("insert", map[string]any{"namespace": "sec", "table": "creds", "records": []map[string]any{
		{"label": "alpha", "token": conformanceSecret, "emb": []float64{1, 0, 0}},
		{"label": "empty", "emb": []float64{0, 1, 0}},
	}})
	h.mustHTTP("update", map[string]any{"namespace": "sec", "table": "creds", "filter": "label = ?", "args": []any{"empty"}, "set": map[string]any{"label": "nulled"}})

	reads := map[string]struct {
		op   string
		body map[string]any
		key  string
	}{
		"read_rows":       {"read_rows", map[string]any{"namespace": "sec", "table": "creds", "ids": []any{1, 2}}, "rows"},
		"search_fulltext": {"search_fulltext", map[string]any{"namespace": "sec", "table": "creds", "query": "alpha OR nulled"}, "results"},
		"search_vector":   {"search_vector", map[string]any{"namespace": "sec", "table": "creds", "column": "emb", "vector": []float64{1, 0, 0}}, "results"},
		"query":           {"query", map[string]any{"namespace": "sec", "sql": "SELECT * FROM creds"}, "rows"},
	}
	for name, rd := range reads {
		data := h.mustHTTP(rd.op, rd.body)
		assertJSONEqual(t, name+" over MCP vs HTTP", h.mustMCP(rd.op, rd.body), data)
		noPlaintext(t, name, data)
		got := tokensOf(t, data[rd.key])
		if got["alpha"] != secret.Mask || got["nulled"] != nil {
			t.Fatalf("%s: tokens = %v, want the mask and null", name, got)
		}
		if rd.op == "query" {
			continue
		}
		revealBody := map[string]any{"reveal": []string{"token"}}
		for k, v := range rd.body {
			revealBody[k] = v
		}
		revealed := h.mustHTTP(rd.op, revealBody)
		assertJSONEqual(t, name+" reveal over MCP vs HTTP", h.mustMCP(rd.op, revealBody), revealed)
		if got := tokensOf(t, revealed[rd.key]); got["alpha"] != conformanceSecret || got["nulled"] != nil {
			t.Fatalf("%s reveal: tokens = %v", name, got)
		}
	}

	noPlaintext(t, "query aliased", h.mustHTTP("query", map[string]any{"namespace": "sec", "sql": "SELECT token AS t FROM creds"}))
	noPlaintext(t, "changes_since", h.mustHTTP("changes_since", map[string]any{"namespace": "sec", "cursor": "begin"}))
	if n := len(h.mustHTTP("query", map[string]any{"namespace": "sec", "sql": "SELECT id FROM creds WHERE token = ?", "args": []any{conformanceSecret}})["rows"].([]any)); n != 0 {
		t.Fatalf("SQL sees only ciphertext, so a plaintext comparison must match nothing, matched %d", n)
	}

	status, body := h.httpCall("read_rows", map[string]any{"namespace": "sec", "table": "creds", "ids": []any{1}, "reveal": []string{"label"}})
	if status != http.StatusBadRequest {
		t.Fatalf("reveal of a non-secret field: status %d: %v", status, body)
	}
	wantMessage(t, "reveal non-secret", envelopeOf(t, body)["message"].(string), `reveal lists only secret fields`)

	h.ensureNS("sec2")
	for option, pattern := range map[string]string{
		"fulltext":  `full-text index would hold the plaintext`,
		"vectorize": `embedding is computed from the plaintext`,
	} {
		status, body := h.httpCall("create_table", map[string]any{"namespace": "sec2", "table": "bad_" + option, "fields": []map[string]any{{"name": "token", "type": "secret", option: true}}})
		if status != http.StatusBadRequest {
			t.Fatalf("secret with %s: status %d: %v", option, status, body)
		}
		wantMessage(t, "secret "+option, envelopeOf(t, body)["message"].(string), pattern)
	}

	emb := openEmbedded(t, t.TempDir(), dolmen.WithSecretKey(conformanceKey()))
	defer emb.Close()
	ctx := context.Background()
	if err := emb.CreateNamespace(ctx, "sec"); err != nil {
		t.Fatal(err)
	}
	if _, err := emb.CreateTable(ctx, "sec", "creds", []dolmen.Field{{Name: "label", Type: "string"}, {Name: "token", Type: "secret"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := emb.Insert(ctx, "sec", "creds", []map[string]any{{"label": "alpha", "token": conformanceSecret}}, dolmen.InsertOptions{}); err != nil {
		t.Fatal(err)
	}
	masked, err := emb.GetRows(ctx, "sec", "creds", []int64{1})
	if err != nil || masked.Rows[0]["token"] != secret.Mask {
		t.Fatalf("facade masked read: %v %v", masked.Rows, err)
	}
	plain, err := emb.RevealRows(ctx, "sec", "creds", []int64{1}, []string{"token"})
	if err != nil || plain.Rows[0]["token"] != conformanceSecret {
		t.Fatalf("facade reveal: %v %v", plain.Rows, err)
	}
}

func TestSecretRevealRefusedUnderAuth(t *testing.T) {
	h := newHarnessMode(t, authAdminKey)
	status, body := h.httpCallAs(identity{bearer: authAdminKey.adminKey}, "read_rows", map[string]any{"namespace": "sec", "table": "creds", "ids": []any{1}, "reveal": []string{"token"}})
	if status != http.StatusForbidden {
		t.Fatalf("status %d: %v", status, body)
	}
	env := envelopeOf(t, body)
	if env["code"] != "forbidden" {
		t.Fatalf("code = %v", env["code"])
	}
	wantMessage(t, "reveal under auth", env["message"].(string), `reveal verb`)
}
