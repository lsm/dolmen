package embed

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// TestLocalEmbedLive runs the real rembed path end to end: it downloads the
// default model from the Hugging Face Hub on first use (tens of MB) and
// caches it under the data dir. Gated behind DOLMEN_TEST_EMBED_LOCAL=1 so
// the default `make test` stays offline and fast; run it explicitly when
// touching the local provider:
//
//	DOLMEN_TEST_EMBED_LOCAL=1 go test ./internal/embed/ -run TestLocalEmbedLive -v
func TestLocalEmbedLive(t *testing.T) {
	if os.Getenv("DOLMEN_TEST_EMBED_LOCAL") != "1" {
		t.Skip("set DOLMEN_TEST_EMBED_LOCAL=1 to run the live local-provider test (downloads the default model on first use)")
	}

	old, had := os.LookupEnv("REMBED_CACHE")
	t.Cleanup(func() {
		if had {
			os.Setenv("REMBED_CACHE", old)
		} else {
			os.Unsetenv("REMBED_CACHE")
		}
	})
	os.Unsetenv("REMBED_CACHE")

	dataDir := t.TempDir()
	p, err := NewProvider("local", "", "", "", dataDir)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	vecs, err := p.Embed(context.Background(), []string{
		"A cat sat on the mat.",
		"The Senate passed the budget bill.",
	})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if len(vecs) != 2 || len(vecs[0]) != 384 {
		t.Fatalf("MiniLM-L6-v2 must produce 384-dim vectors, got %v", vecs)
	}
	var norm float64
	for _, x := range vecs[0] {
		norm += float64(x) * float64(x)
	}
	if math.Abs(norm-1) > 1e-3 {
		t.Fatalf("MiniLM-L6-v2 embeddings are L2-normalized, got norm %f", math.Sqrt(norm))
	}

	// Retrieval sanity: a cat query must rank the cat sentence over the
	// budget sentence.
	query, err := p.Embed(context.Background(), []string{"a feline pet animal"})
	if err != nil {
		t.Fatalf("embed query: %v", err)
	}
	cat := cosine32(query[0], vecs[0])
	budget := cosine32(query[0], vecs[1])
	if cat <= budget {
		t.Fatalf("cat query must rank the cat sentence first: cat=%f budget=%f", cat, budget)
	}

	// The weights must land in the model cache under the data dir, not
	// beside the binary or in the user cache dir.
	cache := filepath.Join(dataDir, "models", "sentence-transformers--all-MiniLM-L6-v2")
	if fi, err := os.Stat(filepath.Join(cache, "model.safetensors")); err != nil || fi.Size() == 0 {
		t.Fatalf("model weights must be cached under the data dir: %v", err)
	}
}

func cosine32(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
