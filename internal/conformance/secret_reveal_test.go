package conformance

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/secret"
)

func seedRevealHarness(t *testing.T) *harness {
	t.Helper()
	sqliteOnly(t)
	h := newHarnessMode(t, authGateway)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "sec"})
	h.mustHTTP("create_table", map[string]any{"namespace": "sec", "table": "creds", "fields": secretTableFields()})
	h.mustHTTP("insert", map[string]any{"namespace": "sec", "table": "creds", "records": []map[string]any{
		{"label": "alpha", "token": conformanceSecret, "emb": []float64{1, 0, 0}},
	}})
	return h
}

func callAs(t *testing.T, h *harness, who identity, op string, body any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	res, out := h.postNoCredential(t, h.httpURL+"/"+op, string(raw), who.headers())
	return res.StatusCode, out
}

func mcpAs(t *testing.T, h *harness, who identity, op string, args any) mcpResult {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": mcpNextID(), "method": "tools/call", "params": map[string]any{"name": op, "arguments": args}})
	_, out := h.postNoCredential(t, h.mcpURL, string(raw), who.headers())
	res := mcpResult{status: http.StatusOK}
	if e, ok := out["error"].(map[string]any); ok {
		res.proto = e
		return res
	}
	res.result, _ = out["result"].(map[string]any)
	return res
}

func revealRead() map[string]any {
	return map[string]any{"namespace": "sec", "table": "creds", "ids": []any{1}, "reveal": []string{"token"}}
}

func wantRevealDenied(t *testing.T, h *harness, who identity, what string) {
	t.Helper()
	status, body := callAs(t, h, who, "read_rows", revealRead())
	if status != http.StatusForbidden {
		t.Fatalf("%s over HTTP: status %d, want 403: %v", what, status, body)
	}
	env := envelopeOf(t, body)
	wantMessage(t, what+" over HTTP", env["message"].(string), `grant the reveal verb`)
	noPlaintext(t, what, body)
	res := mcpAs(t, h, who, "read_rows", revealRead())
	env = res.toolError()
	if env == nil || env["code"] != "forbidden" {
		t.Fatalf("%s over MCP: want a forbidden tool error, got %+v", what, res)
	}
	wantMessage(t, what+" over MCP", env["message"].(string), `grant the reveal verb`)
}

func wantRevealed(t *testing.T, h *harness, who identity, what string) {
	t.Helper()
	status, body := callAs(t, h, who, "read_rows", revealRead())
	if status != http.StatusOK {
		t.Fatalf("%s over HTTP: status %d: %v", what, status, body)
	}
	data := body["data"].(map[string]any)
	if got := tokensOf(t, data["rows"]); got["alpha"] != conformanceSecret {
		t.Fatalf("%s over HTTP: tokens %v", what, got)
	}
	res := mcpAs(t, h, who, "read_rows", revealRead())
	if res.isError() {
		t.Fatalf("%s over MCP: %+v", what, res)
	}
	assertJSONEqual(t, what+" MCP vs HTTP", res.structured(), data)
}

func TestRevealVerbGatesPlaintext(t *testing.T) {
	h := seedRevealHarness(t)
	alice := identity{principal: "alice"}

	grantTo(t, h, "principal", "alice", "sec", "creds", "read")
	status, body := callAs(t, h, alice, "read_rows", map[string]any{"namespace": "sec", "table": "creds", "ids": []any{1}})
	if status != http.StatusOK || tokensOf(t, body["data"].(map[string]any)["rows"])["alpha"] != secret.Mask {
		t.Fatalf("read without reveal must return the mask: %d %v", status, body)
	}
	wantRevealDenied(t, h, alice, "read without reveal")

	grantTo(t, h, "principal", "alice", "sec", "", "admin")
	wantRevealDenied(t, h, alice, "admin does not imply reveal")
	wantRevealDenied(t, h, identity{bearer: authGateway.adminKey}, "the bootstrap admin key")

	for _, op := range []string{"search_fulltext", "search_vector"} {
		body := map[string]any{"namespace": "sec", "table": "creds", "reveal": []string{"token"}}
		if op == "search_fulltext" {
			body["query"] = "alpha"
		} else {
			body["column"] = "emb"
			body["vector"] = []float64{1, 0, 0}
		}
		if status, out := callAs(t, h, alice, op, body); status != http.StatusForbidden {
			t.Fatalf("%s reveal without the verb: status %d: %v", op, status, out)
		}
	}

	grantTo(t, h, "principal", "alice", "sec", "creds", "reveal")
	wantRevealed(t, h, alice, "table-scope reveal")

	grants := h.mustHTTP("list_grants", map[string]any{"subject": map[string]any{"type": "principal", "id": "alice"}})
	found := false
	for _, g := range grants["grants"].([]any) {
		obj := g.(map[string]any)["object"].(map[string]any)
		if obj["table"] == "creds" {
			verbs := g.(map[string]any)["verbs"].([]any)
			if len(verbs) != 2 || verbs[0] != "read" || verbs[1] != "reveal" {
				t.Fatalf("list_grants verbs = %v, want [read reveal]", verbs)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("list_grants lost the table grant: %v", grants)
	}

	h.mustHTTP("revoke", map[string]any{"subject": map[string]any{"type": "principal", "id": "alice"}, "object": map[string]any{"namespace": "sec", "table": "creds"}, "verbs": []string{"reveal"}})
	wantRevealDenied(t, h, alice, "after revoking reveal")

	bob, carol := identity{principal: "bob"}, identity{principal: "carol"}
	grantTo(t, h, "principal", "bob", "sec", "", "read", "reveal")
	wantRevealed(t, h, bob, "namespace-scope reveal")
	grantTo(t, h, "principal", "carol", "*", "", "read", "reveal")
	wantRevealed(t, h, carol, "server-scope reveal")
	h.mustHTTP("revoke", map[string]any{"subject": map[string]any{"type": "principal", "id": "carol"}, "object": map[string]any{"namespace": "*"}, "verbs": []string{"reveal"}})
	wantRevealDenied(t, h, carol, "after revoking server-scope reveal")
}

func TestRevealStaysWithinTheRowScope(t *testing.T) {
	sqliteOnly(t)
	h := newHarnessMode(t, authGateway)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "sec"})
	h.mustHTTP("create_table", map[string]any{"namespace": "sec", "table": "creds", "fields": secretTableFields(), "row_access": "own"})
	h.mustHTTP("insert", map[string]any{"namespace": "sec", "table": "creds", "records": []map[string]any{{"label": "theirs", "token": "their-PLAINTEXT"}}})
	erin := identity{principal: "erin"}
	grantTo(t, h, "principal", "erin", "sec", "creds", "create", "reveal")
	if status, out := callAs(t, h, erin, "insert", map[string]any{"namespace": "sec", "table": "creds", "records": []map[string]any{{"label": "mine", "token": "mine-plain"}}}); status != http.StatusOK {
		t.Fatalf("insert: %d %v", status, out)
	}
	body := map[string]any{"namespace": "sec", "table": "creds", "ids": []any{1, 2}, "reveal": []string{"token"}}
	status, out := callAs(t, h, erin, "read_rows", body)
	if status != http.StatusOK {
		t.Fatalf("own-row reveal: %d %v", status, out)
	}
	rows := out["data"].(map[string]any)["rows"]
	noPlaintext(t, "an own-row reveal", rows)
	if got := tokensOf(t, rows); len(got) != 1 || got["mine"] != "mine-plain" {
		t.Fatalf("own-row reveal returned %v, want only erin's row in plaintext", got)
	}
	assertJSONEqual(t, "own-row reveal MCP vs HTTP", mcpAs(t, h, erin, "read_rows", body).structured(), out["data"].(map[string]any))

	grantTo(t, h, "principal", "erin", "sec", "creds", "read")
	_, out = callAs(t, h, erin, "read_rows", body)
	if got := tokensOf(t, out["data"].(map[string]any)["rows"]); got["theirs"] != "their-PLAINTEXT" || got["mine"] != "mine-plain" {
		t.Fatalf("a table-wide reveal with table-wide read returned %v", got)
	}
}

func TestRevealWritesAnAuditLine(t *testing.T) {
	h := seedRevealHarness(t)
	grantTo(t, h, "principal", "alice", "sec", "creds", "read", "reveal")
	alice := identity{principal: "alice"}
	logs := captureLogs(t, func() {
		status, out := callAs(t, h, alice, "read_rows", revealRead())
		if status != http.StatusOK {
			t.Fatalf("reveal: %d %v", status, out)
		}
	})
	if strings.Contains(logs, "PLAINTEXT") {
		t.Fatalf("the audit log carries the plaintext:\n%s", logs)
	}
	var line string
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, "secret reveal") {
			line = l
		}
	}
	for _, want := range []string{"level=INFO", "principal=alice", "namespace=sec", "table=creds", "row_ids=[1]", "fields=[token]", "request_id="} {
		if !strings.Contains(line, want) {
			t.Fatalf("the reveal audit line %q lacks %q; logs:\n%s", line, want, logs)
		}
	}
	logs = captureLogs(t, func() {
		mcpAs(t, h, alice, "read_rows", revealRead())
	})
	if !strings.Contains(logs, "secret reveal") || !strings.Contains(logs, "principal=alice") || strings.Contains(logs, "PLAINTEXT") {
		t.Fatalf("an MCP reveal must write the same audit line:\n%s", logs)
	}

	off := newHarness(t)
	off.seedTable("sec", "creds", secretTableFields())
	off.mustHTTP("insert", map[string]any{"namespace": "sec", "table": "creds", "records": []map[string]any{{"label": "alpha", "token": conformanceSecret}}})
	logs = captureLogs(t, func() { off.mustHTTP("read_rows", revealRead()) })
	line = ""
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, "secret reveal") {
			line = l
		}
	}
	if line == "" || strings.Contains(logs, "PLAINTEXT") || strings.Contains(line, "principal=") {
		t.Fatalf("an auth-off reveal logs the line without a principal:\n%s", logs)
	}
}

func wantRevealServerError(t *testing.T, h *harness, what, logWant string) {
	t.Helper()
	var status int
	var body map[string]any
	logs := captureLogs(t, func() { status, body = h.httpCall("read_rows", revealRead()) })
	if status != http.StatusInternalServerError {
		t.Fatalf("%s: status %d, want 500: %v", what, status, body)
	}
	env := envelopeOf(t, body)
	if env["code"] != "internal_error" || env["message"] != "internal error" {
		t.Fatalf("%s: envelope %v", what, env)
	}
	if !strings.Contains(logs, logWant) {
		t.Fatalf("%s: the server log does not teach %q:\n%s", what, logWant, logs)
	}
	res := h.mcpCall("read_rows", revealRead())
	if env := res.toolError(); env == nil || env["code"] != "internal_error" {
		t.Fatalf("%s over MCP: %+v", what, res)
	}
}

func TestRevealKeyFailuresAreInternalErrors(t *testing.T) {
	sqliteOnly(t)
	h := newHarness(t)
	h.seedTable("sec", "creds", secretTableFields())
	h.mustHTTP("insert", map[string]any{"namespace": "sec", "table": "creds", "records": []map[string]any{{"label": "alpha", "token": conformanceSecret}}})

	other, err := secret.New(bytes.Repeat([]byte{9}, secret.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	h.secretKeySet, h.secretKey = true, other
	h.reopen()
	wantRevealServerError(t, h, "wrong key", secret.EnvKey)
	wantRevealServerError(t, h, "wrong key", "key id")

	h.secretKey = nil
	h.reopen()
	wantRevealServerError(t, h, "missing key", secret.EnvKey)

	h.secretKey = conformanceKeyring(t)
	h.srv.Close()
	if err := h.st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(h.dir, "sec.db"))
	if err != nil {
		t.Fatal(err)
	}
	var blob []byte
	if err := db.QueryRowContext(context.Background(), "SELECT token FROM creds WHERE id = 1").Scan(&blob); err != nil {
		t.Fatal(err)
	}
	blob[len(blob)-1] ^= 0xff
	if _, err := db.ExecContext(context.Background(), "UPDATE creds SET token = ? WHERE id = 1", blob); err != nil {
		t.Fatal(err)
	}
	db.Close()
	h.start()
	wantRevealServerError(t, h, "tampered value", "tamper")
}
