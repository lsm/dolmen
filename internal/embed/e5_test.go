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
		// Only the model's own name segment decides — not org or parent dirs.
		{"/opt/e5-cache/all-MiniLM-L6-v2", false},
		{"e5lab/model-x", false},
		// Instruct-tuned e5 variants take task instructions, not these prefixes.
		{"intfloat/e5-mistral-7b-instruct", false},
		{"intfloat/multilingual-e5-large-instruct", false},
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

// TestE5IdentityMarker pins the identity versioning: e5-configured servers
// carry a "#e5" marker, so tables embedded before prefixes were applied no
// longer match and are re-embedded via migrate instead of mixing
// representations. Symmetric models keep their long-standing identity.
func TestE5IdentityMarker(t *testing.T) {
	if got := (&Local{Model: "intfloat/multilingual-e5-small"}).Identity(); got != "local/intfloat/multilingual-e5-small#e5" {
		t.Fatalf("Local e5 identity: got %q, want the #e5 marker", got)
	}
	if got := (&Local{Model: "sentence-transformers/all-MiniLM-L6-v2"}).Identity(); got != "local/sentence-transformers/all-MiniLM-L6-v2" {
		t.Fatalf("Local symmetric identity must be unchanged, got %q", got)
	}
	if got := (&OpenAI{BaseURL: "http://localhost:11434/v1", Model: "intfloat/multilingual-e5-small"}).Identity(); got != "openai|http://localhost:11434/v1|intfloat/multilingual-e5-small#e5" {
		t.Fatalf("OpenAI e5 identity: got %q, want the #e5 marker", got)
	}
	if got := (&OpenAI{BaseURL: "http://localhost:11434/v1", Model: "nomic-embed-text"}).Identity(); got != "openai|http://localhost:11434/v1|nomic-embed-text" {
		t.Fatalf("OpenAI symmetric identity must be unchanged, got %q", got)
	}
}

// TestIdentityNoModelCollision pins identity injectivity: a model directory
// whose name already ends in a literal "#e5" (not e5-detected — "#" breaks
// the name segment) must never share an identity with an e5-detected
// directory whose marker produces the same suffix. The model component is
// percent-escaped, so no model reference can contain the marker's bytes.
func TestIdentityNoModelCollision(t *testing.T) {
	e5Dir := (&Local{Model: "/models/foo-e5"}).Identity()         // e5-detected, marker appended
	literalDir := (&Local{Model: "/models/foo-e5#e5"}).Identity() // not detected, literal "#e5" in name
	if e5Dir == literalDir {
		t.Fatalf("identities collide: %q vs %q — the embed_space guard would mix differently preprocessed embeddings", e5Dir, literalDir)
	}
	if want := "local//models/foo-e5#e5"; e5Dir != want {
		t.Fatalf("e5-detected directory identity: got %q, want %q", e5Dir, want)
	}
	if want := "local//models/foo-e5%23e5"; literalDir != want {
		t.Fatalf("literal-#e5 directory identity must escape the marker byte: got %q, want %q", literalDir, want)
	}
	// "%" itself is escaped too, keeping the encoding injective.
	if got, want := (&Local{Model: "/models/100%23e5"}).Identity(), "local//models/100%2523e5"; got != want {
		t.Fatalf("percent in model must be escaped: got %q, want %q", got, want)
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
