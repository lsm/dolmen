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

func TestSecretFieldContract(t *testing.T) {
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

	wantNoPlaintextInStorage(t, h)

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

	h.mustHTTP("insert", map[string]any{"namespace": "sec", "table": "creds", "records": []map[string]any{{"label": "control-PLAINTEXT"}}})
	if len(storageHoldsPlaintext(t, h)) == 0 {
		t.Fatal("the storage scan misses plaintext in an ordinary string field, so its clean result above proves nothing")
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

func TestWritingTheMaskBackNeverReplacesASecret(t *testing.T) {
	h := newHarness(t)
	h.seedTable("sec", "creds", secretTableFields())
	h.mustHTTP("insert", map[string]any{"namespace": "sec", "table": "creds", "records": []map[string]any{
		{"label": "alpha", "token": conformanceSecret, "emb": []float64{1, 0, 0}},
	}})

	writes := []struct {
		name string
		op   string
		body map[string]any
	}{
		{"insert", "insert", map[string]any{"namespace": "sec", "table": "creds", "records": []map[string]any{{"label": "beta", "token": secret.Mask, "emb": []float64{0, 1, 0}}}}},
		{"update", "update", map[string]any{"namespace": "sec", "table": "creds", "filter": "label = ?", "args": []any{"alpha"}, "set": map[string]any{"token": secret.Mask}}},
		{"upsert", "upsert", map[string]any{"namespace": "sec", "table": "creds", "filter": "label = ?", "args": []any{"alpha"}, "set": map[string]any{"token": secret.Mask}}},
		{"upsert_by_key", "upsert_by_key", map[string]any{"namespace": "sec", "table": "creds", "on": []string{"label"}, "records": []map[string]any{{"label": "alpha", "token": secret.Mask}}}},
	}
	for _, w := range writes {
		status, out := h.httpCall(w.op, w.body)
		if status != http.StatusBadRequest {
			t.Fatalf("%s wrote the mask back as a secret value: status %d %v", w.name, status, out)
		}
		errEnv, _ := out["error"].(map[string]any)
		msg, _ := errEnv["message"].(string)
		if !strings.Contains(msg, "token") || !strings.Contains(msg, "reveal") {
			t.Fatalf("%s: the refusal must name the field and the way to read the real value: %q", w.name, msg)
		}
	}

	revealed := h.mustHTTP("read_rows", map[string]any{"namespace": "sec", "table": "creds", "ids": []any{1}, "reveal": []string{"token"}})
	if got := tokensOf(t, revealed["rows"]); got["alpha"] != conformanceSecret {
		t.Fatalf("a refused write still replaced the stored secret: %v", got)
	}
}
