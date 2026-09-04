package embed

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeEngine stands in for rembed's *Embedder so provider logic is testable
// without a model download.
type fakeEngine struct {
	dim float32
}

func (f fakeEngine) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{f.dim, f.dim}
	}
	return out, nil
}

func TestNewProviderLocalDefaults(t *testing.T) {
	p, err := NewProvider("local", "", "", "", "")
	if err != nil {
		t.Fatalf("NewProvider(local): %v", err)
	}
	l, ok := p.(*Local)
	if !ok {
		t.Fatalf("expected *Local, got %T", p)
	}
	if l.Model != "sentence-transformers/all-MiniLM-L6-v2" {
		t.Fatalf("default model must be MiniLM-L6-v2, got %q", l.Model)
	}
	if l.Name() != "local" {
		t.Fatalf("Name: %q", l.Name())
	}
	if got, want := l.Identity(), "local/sentence-transformers/all-MiniLM-L6-v2"; got != want {
		t.Fatalf("Identity: got %q want %q", got, want)
	}
}

func TestNewProviderLocalModelValidation(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		model   string
		wantErr string
	}{
		{"sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2", ""},
		{"BAAI/bge-small-en-v1.5", ""},
		{dir, ""},
		{"MiniLM", "neither a Hugging Face model id"},
		{"org/name/extra", "neither a Hugging Face model id"},
		{"../escape/hatch", `must not contain ".."`},
		{"org/../name", `must not contain ".."`},
		{filepath.Join(dir, "missing"), "not an existing model directory"},
	}
	for _, tc := range cases {
		_, err := NewProvider("local", "", tc.model, "", "")
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("model %q: unexpected error %v", tc.model, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("model %q: want error containing %q, got %v", tc.model, tc.wantErr, err)
		}
	}
}

func TestNewProviderLocalCacheEnv(t *testing.T) {
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
	if _, err := NewProvider("local", "", "", "", dataDir); err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	want := filepath.Join(dataDir, localModelDir)
	if got := os.Getenv("REMBED_CACHE"); got != want {
		t.Fatalf("REMBED_CACHE: got %q want %q", got, want)
	}
	if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
		t.Fatalf("model cache dir must be created: %v", err)
	}

	// An operator-set cache must win over the data-dir default.
	os.Setenv("REMBED_CACHE", "/operator/cache")
	if _, err := NewProvider("local", "", "", "", dataDir); err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if got := os.Getenv("REMBED_CACHE"); got != "/operator/cache" {
		t.Fatalf("explicit REMBED_CACHE must not be overridden, got %q", got)
	}
}

func TestLocalEmbedLazyRetryAndSuccess(t *testing.T) {
	calls := 0
	boom := errors.New("boom")
	l := &Local{
		Model: "org/model",
		Open: func() (LocalEngine, error) {
			calls++
			if calls == 1 {
				return nil, boom
			}
			return fakeEngine{dim: 0.5}, nil
		},
	}
	_, firstErr := l.Embed(context.Background(), []string{"a"})
	if firstErr == nil || !errors.Is(firstErr, boom) {
		t.Fatalf("first Embed must fail with the load error, got %v", firstErr)
	}
	for _, want := range []string{"org/model", "Hugging Face Hub"} {
		if !strings.Contains(firstErr.Error(), want) {
			t.Fatalf("load error must mention %q to orient the operator, got %q", want, firstErr)
		}
	}
	vecs, err := l.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("a failed load must be retried, not memoized: %v", err)
	}
	if len(vecs) != 2 || vecs[0][0] != 0.5 {
		t.Fatalf("unexpected vectors %v", vecs)
	}
	if calls != 2 {
		t.Fatalf("Open must be called once per failed attempt, got %d", calls)
	}
}

func TestLocalEmbedLoadsOnce(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	l := &Local{
		Model: "org/model",
		Open: func() (LocalEngine, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			return fakeEngine{dim: 1}, nil
		},
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			vecs, err := l.Embed(context.Background(), []string{"x"})
			if err != nil || len(vecs) != 1 || vecs[0][0] != 1 {
				t.Errorf("concurrent Embed: %v %v", vecs, err)
			}
		}()
	}
	wg.Wait()
	if calls != 1 {
		t.Fatalf("engine must load exactly once under concurrent Embed calls, loaded %d times", calls)
	}
}

func TestLocalIdentityIncludesDirectoryRef(t *testing.T) {
	l := &Local{Model: "/opt/models/minilm"}
	if got, want := l.Identity(), "local//opt/models/minilm"; got != want {
		t.Fatalf("Identity: got %q want %q", got, want)
	}
}

func TestLocalRef(t *testing.T) {
	cases := map[string]string{
		"sentence-transformers/all-MiniLM-L6-v2": "hf:sentence-transformers/all-MiniLM-L6-v2",
		"intfloat/multilingual-e5-small":         "hf:intfloat/multilingual-e5-small", // any org/name is a Hub ref
		"/opt/models/minilm":                     "/opt/models/minilm",
		"/opt/models/nested/dir":                 "/opt/models/nested/dir",
	}
	for model, want := range cases {
		if got := localRef(model); got != want {
			t.Errorf("localRef(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestLocalEmbedContextCanceled(t *testing.T) {
	l := &Local{Model: "org/model", Open: func() (LocalEngine, error) { return fakeEngine{}, nil }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.Embed(ctx, []string{"a"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context must surface before any load, got %v", err)
	}
}
