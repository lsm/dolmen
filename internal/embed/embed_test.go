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

func TestOpenAINullEmbeddingEntryRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": []any{1, nil, 2}}},
		})
	}))
	defer srv.Close()
	p := &OpenAI{BaseURL: srv.URL, Model: "m", APIKey: ""}
	if _, err := p.Embed(context.Background(), []string{"a"}); err == nil {
		t.Fatal("expected null embedding entry to be rejected")
	}
}

func TestOpenAIOutOfRangeEmbeddingRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": []any{3.5e38}}},
		})
	}))
	defer srv.Close()
	p := &OpenAI{BaseURL: srv.URL, Model: "m", APIKey: ""}
	if _, err := p.Embed(context.Background(), []string{"a"}); err == nil {
		t.Fatal("expected out-of-float32-range embedding value to be rejected")
	}
}

func TestOpenAIRaggedBatchRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"index": 0, "embedding": []float64{0.1, 0.2, 0.3}},
				{"index": 1, "embedding": []float64{0.4, 0.5}},
			},
		})
	}))
	defer srv.Close()
	p := &OpenAI{BaseURL: srv.URL, Model: "m", APIKey: ""}
	if _, err := p.Embed(context.Background(), []string{"a", "b"}); err == nil {
		t.Fatal("expected ragged batch to be rejected")
	}
}

func TestOpenAIRaggedAcrossBatchesRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		data := make([]map[string]any, len(req.Input))
		dims := []int{2, 2, 3}
		for i := range req.Input {
			vec := make([]float64, dims[i%len(dims)])
			for j := range vec {
				vec[j] = 0.1
			}
			data[i] = map[string]any{"index": i, "embedding": vec}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()
	p := &OpenAI{BaseURL: srv.URL, Model: "m", APIKey: ""}
	texts := make([]string, openAIBatch+1)
	if _, err := p.Embed(context.Background(), texts); err == nil {
		t.Fatal("expected ragged vectors across HTTP batches to be rejected")
	}
}

func TestOpenAIMissingIndexRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"embedding": []float64{0.1, 0.2}}},
		})
	}))
	defer srv.Close()
	p := &OpenAI{BaseURL: srv.URL, Model: "m", APIKey: ""}
	if _, err := p.Embed(context.Background(), []string{"a"}); err == nil {
		t.Fatal("expected an embedding without an index to be rejected")
	}
}

func TestNewProviderOpenAIEmptyKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("no Authorization header expected for empty key")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": []float64{0.1, 0.2}}},
		})
	}))
	defer srv.Close()

	p, err := NewProvider("openai", srv.URL, "m", "", "")
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	pAI, ok := p.(*OpenAI)
	if !ok {
		t.Fatalf("expected *OpenAI, got %T", p)
	}
	if pAI.APIKey != "" {
		t.Fatalf("explicit empty API key must be preserved, got %q", pAI.APIKey)
	}
	if _, err := pAI.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatalf("embed: %v", err)
	}
}

func TestProviderModelName(t *testing.T) {
	if got := (None{}).ModelName(); got != "" {
		t.Fatalf("none must report no model, got %q", got)
	}
	if got := (&OpenAI{Model: "text-embedding-3-small"}).ModelName(); got != "text-embedding-3-small" {
		t.Fatalf("openai must report the configured model, got %q", got)
	}
}

func TestOpenAIIdentityRedactsUserinfo(t *testing.T) {
	cases := []struct {
		baseURL string
		want    string
	}{
		{"https://user:secret@proxy.example/v1", "openai|https://proxy.example/v1|m"},
		{"https://user@proxy.example/v1/", "openai|https://proxy.example/v1|m"},
		{"https://api.openai.com/v1", "openai|https://api.openai.com/v1|m"},
	}
	for _, tc := range cases {
		if got := (&OpenAI{BaseURL: tc.baseURL, Model: "m"}).Identity(); got != tc.want {
			t.Fatalf("Identity() with base URL %q = %q, want %q", tc.baseURL, got, tc.want)
		}
	}
}
