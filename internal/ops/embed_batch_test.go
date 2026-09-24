package ops

import (
	"context"
	"testing"
)

type countingProvider struct{ calls []int }

func (p *countingProvider) Identity() string { return "count" }

func (p *countingProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	p.calls = append(p.calls, len(texts))
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{float32(len(texts[i]))}
	}
	return out, nil
}

func (p *countingProvider) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	return []float32{1}, nil
}

func TestEmbeddingIsSentInBoundedBatchesInOrder(t *testing.T) {
	p := &countingProvider{}
	texts := make([]string, 150)
	for i := range texts {
		texts[i] = string(make([]byte, i))
	}
	vecs, err := Embedder(p).Embed(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.calls) != 3 || p.calls[0] != EmbedBatchSize || p.calls[2] != 150-2*EmbedBatchSize {
		t.Fatalf("150 texts went to the provider as %v; want batches of at most %d", p.calls, EmbedBatchSize)
	}
	for i, v := range vecs {
		if v[0] != float32(i) {
			t.Fatalf("vector %d belongs to text %v; batching must keep order", i, v[0])
		}
	}
}
