package conformance

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func captureLogs(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)
	fn()
	return buf.String()
}

func TestDenialLogsTheResolvedPrincipal(t *testing.T) {
	h := newHarnessMode(t, authGateway)

	logs := captureLogs(t, func() {
		res, out := h.postNoCredential(t, h.httpURL+"/list_namespaces", "{}", aliceHeaders())
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("expected a 403 to log: status %d %v", res.StatusCode, out)
		}
	})
	if !strings.Contains(logs, "principal=alice") {
		t.Fatalf("a 403 logged without the resolved principal, so the denial has no audit trail:\n%s", logs)
	}

	logs = captureLogs(t, func() {
		res, out := h.postNoCredential(t, h.mcpURL,
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_namespaces","arguments":{}}}`,
			aliceHeaders())
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("expected a 403 over MCP: status %d %v", res.StatusCode, out)
		}
	})
	if !strings.Contains(logs, "principal=alice") {
		t.Fatalf("an MCP 403 logged without the resolved principal:\n%s", logs)
	}
}

func TestUnauthenticatedDenialLogsNoPrincipal(t *testing.T) {
	h := newHarnessMode(t, authGateway)

	logs := captureLogs(t, func() {
		res, out := h.postNoCredential(t, h.httpURL+"/list_namespaces", "{}")
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected a 401: status %d %v", res.StatusCode, out)
		}
	})
	if strings.Contains(logs, "principal=") {
		t.Fatalf("a 401 logged a principal, but no identity was resolved:\n%s", logs)
	}
}

func TestResponseBodiesNeverEchoIdentity(t *testing.T) {
	h := newHarnessMode(t, authGateway)

	res, out := h.postNoCredential(t, h.httpURL+"/list_namespaces", "{}", aliceHeaders())
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403: %v", res.StatusCode, out)
	}
	body := mustJSON(t, out)
	for _, leak := range []string{"alice", "team-a", "readers"} {
		if strings.Contains(body, leak) {
			t.Fatalf("the denial body echoes caller-supplied identity %q: %s", leak, body)
		}
	}
}
