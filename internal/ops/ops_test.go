package ops

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type fakeProvider struct {
	identity string
	query    []float32
	fail     error
}

func (p *fakeProvider) Identity() string { return p.identity }

func (p *fakeProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 2, 3, 4}
	}
	return out, nil
}

func (p *fakeProvider) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	if p.fail != nil {
		return nil, p.fail
	}
	return p.query, nil
}

func newEngine(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func mustVectorTable(t *testing.T, eng *store.Store, ns, table string) {
	t.Helper()
	if err := EnsureNamespace(context.Background(), eng, ns); err != nil {
		t.Fatalf("ensure namespace: %v", err)
	}
	if _, err := eng.CreateTable(context.Background(), ns, table, []schema.Field{
		{Name: "body", Type: schema.Text, Vectorize: true},
	}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("create table: %v", err)
	}
}

func TestEnsureNamespaceCreatesThenNoOps(t *testing.T) {
	eng := newEngine(t)
	ctx := context.Background()
	if err := EnsureNamespace(ctx, eng, "app"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := EnsureNamespace(ctx, eng, "app"); err != nil {
		t.Fatalf("second ensure must treat an existing namespace as success, got %v", err)
	}
	nss, err := eng.ListNamespaces(ctx, "", nil)
	if err != nil || len(nss) != 1 || nss[0] != "app" {
		t.Fatalf("expected exactly namespace app, got %v (%v)", nss, err)
	}
}

func TestEnsureNamespacePassesThroughOtherErrors(t *testing.T) {
	eng := newEngine(t)
	err := EnsureNamespace(context.Background(), eng, "Bad/Path")
	if err == nil || errors.Is(err, store.ErrExists) {
		t.Fatalf("invalid namespace must fail, got %v", err)
	}
}

func TestPrepareVectorQueryTextEmbedsAndPinsIdentity(t *testing.T) {
	eng := newEngine(t)
	ctx := context.Background()
	mustVectorTable(t, eng, "app", "docs")
	p := &fakeProvider{identity: "fake|v1", query: []float32{1, 2, 3, 4}}
	vq, err := PrepareVectorQuery(ctx, eng, "app", "docs", VectorQuery{Text: "hello"}, p, "help text")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if vq.EmbedModel != "fake|v1" {
		t.Fatalf("text queries must pin the provider identity, got %q", vq.EmbedModel)
	}
	if len(vq.Vec) != 4 {
		t.Fatalf("expected embedded query vector, got %v", vq.Vec)
	}
	if vq.Column != "" {
		t.Fatalf("an empty column must stay empty for the engine to resolve the vectorize space, got %q", vq.Column)
	}
}

func TestPrepareVectorQueryRawVectorCarriesNoIdentity(t *testing.T) {
	eng := newEngine(t)
	ctx := context.Background()
	mustVectorTable(t, eng, "app", "docs")
	p := &fakeProvider{identity: "fake|v1", query: []float32{1, 2, 3, 4}}
	vq, err := PrepareVectorQuery(ctx, eng, "app", "docs", VectorQuery{Vec: []float64{1, 2, 3, 4}}, p, "help text")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if vq.EmbedModel != "" {
		t.Fatalf("raw vectors must not claim a provider identity, got %q", vq.EmbedModel)
	}
	if len(vq.Vec) != 4 || vq.Vec[3] != 4 {
		t.Fatalf("expected converted vector, got %v", vq.Vec)
	}
}

func TestPrepareVectorQueryShapeErrors(t *testing.T) {
	eng := newEngine(t)
	ctx := context.Background()
	mustVectorTable(t, eng, "app", "docs")
	p := &fakeProvider{identity: "fake|v1", query: []float32{1, 2, 3, 4}}

	_, err := PrepareVectorQuery(ctx, eng, "app", "docs", VectorQuery{Text: "x", Vec: []float64{1}}, p, "help")
	if err == nil || !strings.Contains(err.Error(), "pass either text or vector, not both") {
		t.Fatalf("expected both-fields rejection, got %v", err)
	}
	if code := sharedCode(err); code != derr.InvalidRequest {
		t.Fatalf("expected invalid_request, got %v", code)
	}

	_, err = PrepareVectorQuery(ctx, eng, "app", "docs", VectorQuery{}, p, "help")
	if err == nil || !strings.Contains(err.Error(), "pass either text or vector") {
		t.Fatalf("expected missing-input rejection, got %v", err)
	}

	_, err = PrepareVectorQuery(ctx, eng, "app", "docs", VectorQuery{Vec: []float64{1, math.NaN()}}, p, "help")
	if err == nil || !strings.Contains(err.Error(), "outside the float32 range") {
		t.Fatalf("expected range rejection, got %v", err)
	}

	_, err = PrepareVectorQuery(ctx, eng, "app", "docs", VectorQuery{Vec: []float64{1, 1e300}}, p, "help")
	if err == nil || !strings.Contains(err.Error(), "outside the float32 range") {
		t.Fatalf("expected range rejection, got %v", err)
	}
}

func TestPrepareVectorQueryRejectsTextWithoutProvider(t *testing.T) {
	eng := newEngine(t)
	ctx := context.Background()
	mustVectorTable(t, eng, "app", "docs")
	p := &fakeProvider{identity: "", query: []float32{1}}
	_, err := PrepareVectorQuery(ctx, eng, "app", "docs", VectorQuery{Text: "hello"}, p, "configure it with WithEmbedding")
	if err == nil {
		t.Fatal("text queries without a usable provider must be rejected")
	}
	if !strings.Contains(err.Error(), "no usable embedding provider") || !strings.Contains(err.Error(), "configure it with WithEmbedding") {
		t.Fatalf("message must carry the shared core and the caller-supplied help, got %q", err.Error())
	}
	if code := sharedCode(err); code != derr.InvalidRequest {
		t.Fatalf("expected invalid_request, got %v", code)
	}
}

func TestPrepareVectorQueryRejectsZeroDimensionalResult(t *testing.T) {
	eng := newEngine(t)
	ctx := context.Background()
	mustVectorTable(t, eng, "app", "docs")
	p := &fakeProvider{identity: "fake|v1", query: nil}
	_, err := PrepareVectorQuery(ctx, eng, "app", "docs", VectorQuery{Text: "hello"}, p, "help")
	if err == nil || !strings.Contains(err.Error(), "zero-dimensional") {
		t.Fatalf("expected zero-dimensional rejection, got %v", err)
	}
}

func TestPrepareVectorQueryPassesProviderFailureThrough(t *testing.T) {
	eng := newEngine(t)
	ctx := context.Background()
	mustVectorTable(t, eng, "app", "docs")
	boom := errors.New("provider down")
	p := &fakeProvider{identity: "fake|v1", fail: boom}
	_, err := PrepareVectorQuery(ctx, eng, "app", "docs", VectorQuery{Text: "hello"}, p, "help")
	if !errors.Is(err, boom) {
		t.Fatalf("provider failures must pass through unwrapped for transport classification, got %v", err)
	}
}

func TestEmbedderAdaptsProvider(t *testing.T) {
	p := &fakeProvider{identity: "fake|v1"}
	emb := Embedder(p)
	if emb.Identity != "fake|v1" {
		t.Fatalf("expected adapted identity, got %q", emb.Identity)
	}
	vecs, err := emb.Embed(context.Background(), []string{"a"})
	if err != nil || len(vecs) != 1 {
		t.Fatalf("expected adapted embed call, got %v (%v)", vecs, err)
	}
}

func sharedCode(err error) derr.Code {
	var de *derr.Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}
