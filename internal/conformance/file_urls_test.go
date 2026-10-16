package conformance

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lsm/dolmen/internal/files"
	"github.com/lsm/dolmen/internal/store"
)

type conformanceURLSource struct {
	*files.Request
	calls   *atomic.Int64
	offline *atomic.Bool
}

func (s *conformanceURLSource) Fetch(ctx context.Context, _ string) (string, error) {
	s.calls.Add(1)
	if s.offline.Load() {
		return "", fmt.Errorf("%w: the source has expired", store.ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := s.Receive("remote", "remote.txt", "text/plain", strings.NewReader("remote bytes")); err != nil {
		return "", err
	}
	return "remote", nil
}

func TestFileURLWritesReplayAcrossTransportsWithoutFetching(t *testing.T) {
	if testEngine(t) == store.EnginePostgres {
		t.Skip("files need S3 on PostgreSQL")
	}
	h := newHarness(t)
	handler := h.srv.Config.Handler
	h.srv.Close()
	var calls atomic.Int64
	var offline atomic.Bool
	h.serve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := h.fileStore.NewRequest()
		defer req.Close()
		src := &conformanceURLSource{Request: req, calls: &calls, offline: &offline}
		handler.ServeHTTP(w, r.WithContext(store.WithFileSource(r.Context(), src)))
	}))
	h.seedTable("fil", "docs", fileTableFields())
	rec := map[string]any{"doc": map[string]any{"url": "https://files.example/a.txt", "name": "url.txt"}}
	insert := map[string]any{"namespace": "fil", "table": "docs", "idempotency_key": "url-insert", "records": []any{rec}}
	batch := map[string]any{"namespace": "fil", "idempotency_key": "url-batch", "writes": []any{map[string]any{"kind": "insert", "table": "docs", "records": []any{rec}}}}
	h.mustHTTP("insert", insert)
	h.mustMCP("batch", batch)
	if calls.Load() != 2 {
		t.Fatalf("initial fetches: %d", calls.Load())
	}
	offline.Store(true)
	if out := h.mustMCP("insert", insert); out["replayed"] != true {
		t.Fatalf("MCP replay: %v", out)
	}
	if out := h.mustHTTP("batch", batch); out["replayed"] != true {
		t.Fatalf("HTTP replay: %v", out)
	}
	if calls.Load() != 2 {
		t.Fatalf("replays fetched an expired URL: %d", calls.Load())
	}
	changed := map[string]any{"namespace": "fil", "table": "docs", "idempotency_key": "url-insert", "records": []any{map[string]any{"doc": map[string]any{"url": "https://files.example/a.txt", "name": "changed.txt"}}}}
	if code, out := h.httpCall("insert", changed); code != http.StatusConflict || errCode(out) != "conflict" || calls.Load() != 2 {
		t.Fatalf("changed override: %d %v, fetches %d", code, out, calls.Load())
	}
	insert["idempotency_key"] = "new-url"
	if code, out := h.httpCall("insert", insert); code != http.StatusBadRequest || errCode(out) != "invalid_request" || calls.Load() != 3 {
		t.Fatalf("new key must fetch: %d %v, fetches %d", code, out, calls.Load())
	}
	for _, id := range []string{"1", "2"} {
		res, err := http.Get(h.srv.URL + "/v1/files?namespace=fil&table=docs&id=" + id + "&field=doc")
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK || string(data) != "remote bytes" {
			t.Fatalf("URL download: %d %q", res.StatusCode, data)
		}
	}
}
