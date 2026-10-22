package conformance

import (
	"net/http"
	"testing"

	"github.com/lsm/dolmen/internal/embed"
)

func TestDescribeServerNamesTheFirstUseStateOnEveryTransport(t *testing.T) {
	h := newHarness(t)
	states := map[string]bool{
		embed.ModelStateCached:             true,
		embed.ModelStateDownloadOnFirstUse: true,
		embed.ModelStateIncomplete:         true,
	}

	data, _ := h.mustHTTP("describe_server", map[string]any{})["embedding"].(map[string]any)
	got, _ := data["model_state"].(string)
	if !states[got] {
		t.Fatalf("/v1 describe_server model_state = %q, want one of the three documented states (%v): a caller scanning the response must be able to tell a first run from a fault without reading prose", got, states)
	}
	if _, retired := data["model_cached"]; retired {
		t.Fatalf("/v1 describe_server still reports model_cached, which reads as a fault on a first run: %v", data)
	}
	if usable, _ := data["usable"].(bool); !usable {
		t.Fatalf("usable must still answer whether a vectorized write works, unchanged by the state: %v", data)
	}

	sc := h.mustMCP("describe_server", map[string]any{})
	overMCP, _ := sc["embedding"].(map[string]any)
	mcpState, _ := overMCP["model_state"].(string)
	if mcpState != got {
		t.Fatalf("MCP describe_server model_state = %q, want the same %q as /v1", mcpState, got)
	}
	if _, retired := overMCP["model_cached"]; retired {
		t.Fatalf("MCP describe_server still reports model_cached: %v", overMCP)
	}
}

func TestDescribeServerEmbeddingStatusIsUsableWhileTheModelIsNotYetDownloaded(t *testing.T) {
	h := newHarness(t)
	status, out := h.httpCall("describe_server", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("describe_server status %d, want 200: %v", status, out)
	}
	data, _ := out["data"].(map[string]any)["embedding"].(map[string]any)
	if got, _ := data["model_state"].(string); got != embed.ModelStateDownloadOnFirstUse {
		t.Fatalf("model_state = %q, want %q: this harness has no model on disk, which is a first run and not a fault", got, embed.ModelStateDownloadOnFirstUse)
	}
	if usable, _ := data["usable"].(bool); !usable {
		t.Fatalf("a model that is merely not downloaded yet must leave usable true, so nothing blocks semantic search over it: %v", data)
	}
}
