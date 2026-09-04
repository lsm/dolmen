package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestE5Prefixes(t *testing.T) {
	cases := []struct {
		model string
		isE5  bool
	}{
		{"intfloat/multilingual-e5-small", true},
		{"intfloat/multilingual-e5-base", true},
		{"intfloat/e5-large-v2", true},
		{"intfloat/e5-small-v2", true},
		{"/opt/dolmen/models/intfloat--multilingual-e5-small", true}, // offline dir form
		{"sentence-transformers/all-MiniLM-L6-v2", false},
		{"sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2", false},
		{"BAAI/bge-small-en-v1.5", false}, // asymmetric, but not e5 prefixes
		{"org/5e5", false},                // e5 must be a standalone name segment
		{"org/e5small", false},
		{"org/somee5model", false},
	}
	for _, tc := range cases {
		query, passage := e5Prefixes(tc.model)
		if got := query != "" || passage != ""; got != tc.isE5 {
			t.Errorf("e5Prefixes(%q) = (%q, %q), want e5 detection %v", tc.model, query, passage, tc.isE5)
			continue
		}
		if tc.isE5 && (query != "query: " || passage != "passage: ") {
			t.Errorf("e5Prefixes(%q) = (%q, %q), want the e5 role prefixes", tc.model, query, passage)
		}
	}
}

// recordingEngine captures the text it is asked to embed, so tests can
// assert exactly what reached the engine — the prefix behavior under test.
type recordingEngine struct {
	mu    sync.Mutex
	texts []string
}

func (r *recordingEngine) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	r.mu.Lock()
	r.texts = append(r.texts, texts...)
	r.mu.Unlock()
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

func (r *recordingEngine) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.texts...)
}

func TestLocalE5RolePrefixes(t *testing.T) {
	eng := &recordingEngine{}
	l := &Local{Model: "intfloat/multilingual-e5-small", Open: func() (LocalEngine, error) { return eng, nil }}

	if _, err := l.Embed(context.Background(), []string{"接続プール枯渏", "db down"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if _, err := l.EmbedQuery(context.Background(), "connection pool exhausted"); err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	want := []string{
		"passage: 接続プール枯渏",
		"passage: db down",
		"query: connection pool exhausted",
	}
	got := eng.recorded()
	if len(got) != len(want) {
		t.Fatalf("engine recorded %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("engine text %d = %q, want %q (all: %q)", i, got[i], want[i], got)
		}
	}
}

func TestLocalSymmetricModelGetsNoPrefixes(t *testing.T) {
	eng := &recordingEngine{}
	l := &Local{Model: "sentence-transformers/all-MiniLM-L6-v2", Open: func() (LocalEngine, error) { return eng, nil }}

	if _, err := l.Embed(context.Background(), []string{"a cat"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if _, err := l.EmbedQuery(context.Background(), "feline"); err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	want := []string{"a cat", "feline"}
	if got := eng.recorded(); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("engine recorded %q, want %q", got, want)
	}
}

// openAIRecorder serves the embeddings endpoint and records the input texts
// it receives, so tests can assert what the provider sent.
func openAIRecorder(t *testing.T) (*OpenAI, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		mu.Lock()
		seen = append(seen, body.Input...)
		mu.Unlock()
		data := make([]map[string]any, len(body.Input))
		for i := range data {
			data[i] = map[string]any{"index": i, "embedding": []float64{0.5}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return &OpenAI{BaseURL: srv.URL, Model: "intfloat/multilingual-e5-small", APIKey: ""}, &seen
}

func TestOpenAIE5RolePrefixes(t *testing.T) {
	p, seen := openAIRecorder(t)
	if _, err := p.Embed(context.Background(), []string{"第一条记录"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if _, err := p.EmbedQuery(context.Background(), "first record"); err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	want := []string{"passage: 第一条记录", "query: first record"}
	if len(*seen) != len(want) || (*seen)[0] != want[0] || (*seen)[1] != want[1] {
		t.Fatalf("endpoint received %q, want %q", *seen, want)
	}
}

func TestOpenAISymmetricModelGetsNoPrefixes(t *testing.T) {
	p, seen := openAIRecorder(t)
	p.Model = "text-embedding-3-small"
	if _, err := p.Embed(context.Background(), []string{"plain"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if _, err := p.EmbedQuery(context.Background(), "query text"); err != nil {
		t.Fatalf("EmbedQuery: %v", err)
	}
	want := []string{"plain", "query text"}
	if len(*seen) != len(want) || (*seen)[0] != want[0] || (*seen)[1] != want[1] {
		t.Fatalf("endpoint received %q, want %q", *seen, want)
	}
}
