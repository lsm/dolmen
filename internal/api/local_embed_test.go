package api

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/embed"
	"github.com/lsm/dolmen/internal/store"
)

// localStub returns a Local provider whose engine is swapped for a stub, so
// the full create/insert/search HTTP path runs against the real provider
// wiring (identity, lazy load) without a model download.
func localStub(model string, dim int) *embed.Local {
	return &embed.Local{
		Model: model,
		Open: func() (embed.LocalEngine, error) {
			return stubEngine{dim: dim}, nil
		},
	}
}

type stubEngine struct{ dim int }

func (s stubEngine) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = make([]float32, s.dim)
		for j := range out[i] {
			out[i][j] = 0.1
		}
	}
	return out, nil
}

// TestLocalProviderVectorizeEndToEnd pins the acceptance path of the local
// provider: a server with no external endpoint creates a vectorized table,
// inserts rows (embedding through the provider seam), and answers a text
// search against the server-managed _embedding space.
func TestLocalProviderVectorizeEndToEnd(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewServer(New(st, localStub("sentence-transformers/all-MiniLM-L6-v2", 4)).Handler())
	t.Cleanup(srv.Close)

	code, res := post(t, srv.URL, "create_table", map[string]any{
		"namespace": "app",
		"table":     "docs",
		"fields":    []map[string]any{{"name": "body", "type": "text", "vectorize": true}},
	})
	if code != 200 {
		t.Fatalf("create_table with vectorize on a local-provider server: %d %v", code, res)
	}
	code, res = post(t, srv.URL, "insert", map[string]any{
		"namespace": "app",
		"table":     "docs",
		"records":   []map[string]any{{"body": "first"}, {"body": "second"}},
	})
	if code != 200 {
		t.Fatalf("insert: %d %v", code, res)
	}
	code, res = post(t, srv.URL, "search_vector", map[string]any{
		"namespace": "app", "table": "docs", "text": "first",
	})
	if code != 200 {
		t.Fatalf("search_vector text: %d %v", code, res)
	}
	results, _ := res["data"].(map[string]any)["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("text search must return rows, got %v", res)
	}

	// The table's embedding space is pinned to local/<model>.
	code, res = post(t, srv.URL, "describe_table", map[string]any{"namespace": "app", "table": "docs"})
	if code != 200 {
		t.Fatalf("describe_table: %d %v", code, res)
	}
	table, _ := res["data"].(map[string]any)["table"].(map[string]any)
	if got := table["embed_space"]; got != "local/sentence-transformers/all-MiniLM-L6-v2" {
		t.Fatalf("embed_space must pin the local provider identity, got %v", got)
	}
}

// TestLocalProviderSwitchRejected pins provider-switch rejection parity: a
// table embedded by local/<model-a> refuses inserts from local/<model-b>
// until re-embedded via migrate, exactly as with the OpenAI provider.
func TestLocalProviderSwitchRejected(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srvA := httptest.NewServer(New(st, localStub("org/model-a", 4)).Handler())
	t.Cleanup(srvA.Close)

	code, res := post(t, srvA.URL, "create_table", map[string]any{
		"namespace": "app",
		"table":     "docs",
		"fields":    []map[string]any{{"name": "body", "type": "text", "vectorize": true}},
	})
	if code != 200 {
		t.Fatalf("create_table: %d %v", code, res)
	}
	code, res = post(t, srvA.URL, "insert", map[string]any{
		"namespace": "app", "table": "docs", "records": []map[string]any{{"body": "hello"}},
	})
	if code != 200 {
		t.Fatalf("insert under model-a: %d %v", code, res)
	}

	// Same store, server restarted with a different local model.
	srvB := httptest.NewServer(New(st, localStub("org/model-b", 4)).Handler())
	t.Cleanup(srvB.Close)
	code, res = post(t, srvB.URL, "insert", map[string]any{
		"namespace": "app", "table": "docs", "records": []map[string]any{{"body": "more"}},
	})
	if code != 400 {
		t.Fatalf("insert under a different local model must be rejected, got %d %v", code, res)
	}
	if msg, _ := res["error"].(map[string]any)["message"].(string); !strings.Contains(msg, "embedding provider changed") {
		t.Fatalf("rejection must explain the provider change, got %v", res)
	}
	code, res = post(t, srvB.URL, "search_vector", map[string]any{
		"namespace": "app", "table": "docs", "text": "hello",
	})
	if code != 400 {
		t.Fatalf("text search under a different local model must be rejected, got %d %v", code, res)
	}

	// The re-embed flow clears the pin: set_vectorize off, then on.
	vecOff := map[string]any{"op": "set_vectorize", "name": "body", "value": false}
	code, _ = post(t, srvB.URL, "migrate", map[string]any{
		"namespace": "app", "table": "docs",
		"changes":   []map[string]any{vecOff},
	})
	if code != 200 {
		t.Fatalf("migrate vectorize off: %d", code)
	}
	vecOn := map[string]any{"op": "set_vectorize", "name": "body", "value": true}
	code, res = post(t, srvB.URL, "migrate", map[string]any{
		"namespace": "app", "table": "docs",
		"changes":   []map[string]any{vecOn},
	})
	if code != 200 {
		t.Fatalf("migrate vectorize on under model-b: %d %v", code, res)
	}
	code, res = post(t, srvB.URL, "insert", map[string]any{
		"namespace": "app", "table": "docs", "records": []map[string]any{{"body": "more"}},
	})
	if code != 200 {
		t.Fatalf("insert after re-embed under model-b: %d %v", code, res)
	}
}
