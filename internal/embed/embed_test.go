package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenAIEmptyKeyAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("no Authorization header expected for empty key")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": []float64{0.1, 0.2}}},
		})
	}))
	defer srv.Close()

	p := &OpenAI{BaseURL: srv.URL, Model: "local-model", APIKey: ""}
	vecs, err := p.Embed(context.Background(), []string{"hello"})
	if err != nil {
		t.Fatalf("embed with empty key must work against unauthenticated endpoints: %v", err)
	}
	if len(vecs) != 1 || len(vecs[0]) != 2 {
		t.Fatalf("unexpected vectors: %v", vecs)
	}
}

func TestOpenAIInconsistentIndicesRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"index": 0, "embedding": []float64{0.1}},
				{"index": 2, "embedding": []float64{0.2}},
			},
		})
	}))
	defer srv.Close()
	p := &OpenAI{BaseURL: srv.URL, Model: "m", APIKey: ""}
	if _, err := p.Embed(context.Background(), []string{"a", "b"}); err == nil {
		t.Fatal("expected inconsistent indices to be rejected")
	}
}

func TestOpenAIEmptyEmbeddingRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": []float64{}}},
		})
	}))
	defer srv.Close()
	p := &OpenAI{BaseURL: srv.URL, Model: "m", APIKey: ""}
	if _, err := p.Embed(context.Background(), []string{"a"}); err == nil {
		t.Fatal("expected empty embedding to be rejected")
	}
}
