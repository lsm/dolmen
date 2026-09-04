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
	if l.ModelName() != "sentence-transformers/all-MiniLM-L6-v2" {
		t.Fatalf("ModelName: %q", l.ModelName())
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
	p2, err := NewProvider("local", "", "", "", dataDir)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if got := os.Getenv("REMBED_CACHE"); got != "/operator/cache" {
		t.Fatalf("explicit REMBED_CACHE must not be overridden, got %q", got)
	}
	l2 := p2.(*Local)
	if l2.CacheRoot != "/operator/cache" {
		t.Fatalf("CacheRoot: got %q want %q", l2.CacheRoot, "/operator/cache")
	}
}

func TestLocalCached(t *testing.T) {
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
	l := p.(*Local)
	if l.Cached() {
		t.Fatalf("empty cache must report not cached")
	}

	// Simulate a downloaded model cache.
	cacheDir := filepath.Join(dataDir, localModelDir, "sentence-transformers--all-MiniLM-L6-v2")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "config.json"), []byte(`{"model_type": "bert"}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "tokenizer_config.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write tokenizer_config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "modules.json"), []byte(`[]`), 0o600); err != nil {
		t.Fatalf("write modules: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "vocab.txt"), []byte("[PAD]\n"), 0o600); err != nil {
		t.Fatalf("write vocab: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "model.safetensors"), []byte("weights"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !l.Cached() {
		t.Fatalf("a complete cache must report cached")
	}

	// A fully seeded sharded cache also reports cached, so the startup log
	// does not warn about a Hub download an offline install cannot make.
	shardDir := filepath.Join(dataDir, localModelDir, "org--sharded")
	if err := os.MkdirAll(shardDir, 0o700); err != nil {
		t.Fatalf("mkdir shard dir: %v", err)
	}
	for _, f := range []string{"config.json", "tokenizer_config.json", "tokenizer.json"} {
		if err := os.WriteFile(filepath.Join(shardDir, f), []byte(`{}`), 0o600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	if err := os.WriteFile(filepath.Join(shardDir, "modules.json"), []byte(`[]`), 0o600); err != nil {
		t.Fatalf("write modules: %v", err)
	}
	idx := []byte(`{"weight_map": {"layer.0": "model-00001-of-00001.safetensors"}}`)
	if err := os.WriteFile(filepath.Join(shardDir, "model.safetensors.index.json"), idx, 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(shardDir, "model-00001-of-00001.safetensors"), []byte("shard"), 0o600); err != nil {
		t.Fatalf("write shard: %v", err)
	}
	lSharded := &Local{Model: "org/sharded", CacheRoot: filepath.Join(dataDir, localModelDir)}
	if !lSharded.Cached() {
		t.Fatalf("complete sharded cache must report cached")
	}

	// An absolute model-directory path is its own cache.
	absPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(absPath, "model.safetensors"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	l2 := &Local{Model: absPath}
	if !l2.Cached() {
		t.Fatalf("absolute model directory must report cached")
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
	// A failed load must be classifiable as a LoadError so the API can map
	// it to an actionable embedder_unavailable error, not a bare internal one.
	var le *LoadError
	if !errors.As(firstErr, &le) {
		t.Fatalf("load failure must be a *LoadError, got %T", firstErr)
	}
	if le.Model != "org/model" {
		t.Fatalf("LoadError must carry the model name, got %q", le.Model)
	}
	// Hub ids are public identifiers; directory paths are filesystem layout.
	if !le.IsHubID() {
		t.Fatalf("org/model is a Hub id, IsHubID said false")
	}
	if (&LoadError{Model: "/opt/models/minilm"}).IsHubID() {
		t.Fatalf("a directory path is not a Hub id, IsHubID said true")
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

// seedCache writes the artifact set rembed's directory load needs, minus the
// named files, so tests can build partial caches the way an interrupted
// download or tar extraction would leave behind.
func seedCache(t *testing.T, dir string, skip ...string) {
	t.Helper()
	skipMap := make(map[string]struct{}, len(skip))
	for _, s := range skip {
		skipMap[s] = struct{}{}
	}
	files := map[string]string{
		"config.json":           `{"model_type": "bert"}`,
		"tokenizer_config.json": `{"tokenizer_class": "BertTokenizer"}`,
		"modules.json":          `[]`,
		"vocab.txt":             "[PAD]\n",
		"model.safetensors":     "weights",
	}
	for name, body := range files {
		if _, ok := skipMap[name]; ok {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

func TestSeededCacheDir(t *testing.T) {
	cache := t.TempDir()

	// A pre-seeded model cache is detected when the full artifact set is on
	// disk.
	seeded := filepath.Join(cache, "sentence-transformers--all-MiniLM-L6-v2")
	if err := os.MkdirAll(seeded, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	seedCache(t, seeded)
	if got := seededCacheDir(cache, "sentence-transformers/all-MiniLM-L6-v2"); got != seeded {
		t.Fatalf("seededCacheDir: got %q, want %q", got, seeded)
	}

	// Weights alone are not a loadable cache: an extraction interrupted after
	// model.safetensors but before the tokenizer or module files must fall
	// back to the Hub ref, where rembed can resume what is missing.
	for _, missing := range []string{"config.json", "tokenizer_config.json", "modules.json", "vocab.txt"} {
		partial := filepath.Join(cache, "org--partial")
		if err := os.RemoveAll(partial); err != nil {
			t.Fatalf("clean partial: %v", err)
		}
		if err := os.MkdirAll(partial, 0o755); err != nil {
			t.Fatalf("mkdir partial: %v", err)
		}
		seedCache(t, partial, missing)
		if got := seededCacheDir(cache, "org/partial"); got != "" {
			t.Fatalf("seededCacheDir without %s: got %q, want empty", missing, got)
		}
	}

	// A sharded pre-seeded cache is detected when the index and all of its
	// referenced shards exist.
	sharded := filepath.Join(cache, "org--sharded")
	if err := os.MkdirAll(sharded, 0o755); err != nil {
		t.Fatalf("mkdir sharded: %v", err)
	}
	seedCache(t, sharded, "model.safetensors")
	idx := []byte(`{"weight_map": {"layer.0": "model-00001-of-00002.safetensors", "layer.1": "model-00002-of-00002.safetensors"}}`)
	if err := os.WriteFile(filepath.Join(sharded, "model.safetensors.index.json"), idx, 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
	if got := seededCacheDir(cache, "org/sharded"); got != "" {
		t.Fatalf("seededCacheDir missing shards: got %q, want empty", got)
	}
	if err := os.WriteFile(filepath.Join(sharded, "model-00001-of-00002.safetensors"), []byte("s1"), 0o644); err != nil {
		t.Fatalf("write shard one: %v", err)
	}
	if got := seededCacheDir(cache, "org/sharded"); got != "" {
		t.Fatalf("seededCacheDir incomplete shards: got %q, want empty", got)
	}
	if err := os.WriteFile(filepath.Join(sharded, "model-00002-of-00002.safetensors"), []byte("s2"), 0o644); err != nil {
		t.Fatalf("write shard two: %v", err)
	}
	if got := seededCacheDir(cache, "org/sharded"); got != sharded {
		t.Fatalf("seededCacheDir sharded: got %q, want %q", got, sharded)
	}

	// A missing cache means falling back to the Hub.
	if got := seededCacheDir(cache, "org/not-seeded"); got != "" {
		t.Fatalf("seededCacheDir for missing cache: got %q, want empty", got)
	}

	// Absolute model directories are not cache-looked-up.
	if got := seededCacheDir(cache, "/opt/models/minilm"); got != "" {
		t.Fatalf("seededCacheDir for absolute path: got %q, want empty", got)
	}

	// A manifest naming a module directory requires that module's files: a
	// cache missing 1_Pooling/config.json must fall back to the Hub.
	manifest := filepath.Join(cache, "org--manifest")
	if err := os.MkdirAll(manifest, 0o755); err != nil {
		t.Fatalf("mkdir manifest: %v", err)
	}
	seedCache(t, manifest)
	modules := []byte(`[{"path": "", "type": "sentence_transformers.models.Transformer"}, {"path": "1_Pooling", "type": "sentence_transformers.models.Pooling"}]`)
	if err := os.WriteFile(filepath.Join(manifest, "modules.json"), modules, 0o644); err != nil {
		t.Fatalf("write modules: %v", err)
	}
	if got := seededCacheDir(cache, "org/manifest"); got != "" {
		t.Fatalf("seededCacheDir without 1_Pooling/config.json: got %q, want empty", got)
	}
	if err := os.MkdirAll(filepath.Join(manifest, "1_Pooling"), 0o755); err != nil {
		t.Fatalf("mkdir 1_Pooling: %v", err)
	}
	if err := os.WriteFile(filepath.Join(manifest, "1_Pooling", "config.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write pooling config: %v", err)
	}
	if got := seededCacheDir(cache, "org/manifest"); got != manifest {
		t.Fatalf("seededCacheDir with complete modules: got %q, want %q", got, manifest)
	}

	// A Dense projection head (Gemma's 2_Dense) also needs its own weights.
	gemma := filepath.Join(cache, "org--gemma")
	if err := os.MkdirAll(gemma, 0o755); err != nil {
		t.Fatalf("mkdir gemma: %v", err)
	}
	seedCache(t, gemma)
	gemmaModules := []byte(`[{"path": "1_Pooling", "type": "sentence_transformers.models.Pooling"}, {"path": "2_Dense", "type": "sentence_transformers.models.Dense"}]`)
	if err := os.WriteFile(filepath.Join(gemma, "modules.json"), gemmaModules, 0o644); err != nil {
		t.Fatalf("write gemma modules: %v", err)
	}
	for _, sub := range []string{"1_Pooling", "2_Dense"} {
		if err := os.MkdirAll(filepath.Join(gemma, sub), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
		if err := os.WriteFile(filepath.Join(gemma, sub, "config.json"), []byte(`{}`), 0o644); err != nil {
			t.Fatalf("write %s config: %v", sub, err)
		}
	}
	if got := seededCacheDir(cache, "org/gemma"); got != "" {
		t.Fatalf("seededCacheDir without 2_Dense weights: got %q, want empty", got)
	}
	if err := os.WriteFile(filepath.Join(gemma, "2_Dense", "model.safetensors"), []byte("dense"), 0o644); err != nil {
		t.Fatalf("write dense weights: %v", err)
	}
	if got := seededCacheDir(cache, "org/gemma"); got != gemma {
		t.Fatalf("seededCacheDir with dense weights: got %q, want %q", got, gemma)
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

// TestLocalEmbedCanceledWhileQueuedBehindLoad pins the lock-queue case: a
// request whose context is canceled while waiting behind another request's
// in-flight (failing) load must not start a retry download of its own once
// it finally acquires the lock.
func TestLocalEmbedCanceledWhileQueuedBehindLoad(t *testing.T) {
	var mu sync.Mutex
	opens := 0
	release := make(chan struct{})
	l := &Local{
		Model: "org/model",
		Open: func() (LocalEngine, error) {
			mu.Lock()
			opens++
			mu.Unlock()
			<-release // simulate a slow load: hold the provider lock
			return nil, errors.New("load failed")
		},
	}

	resA := make(chan error, 1)
	go func() {
		_, err := l.Embed(context.Background(), []string{"a"})
		resA <- err
	}()
	// Let A enter Open and hold the lock, then queue B and cancel it while
	// it waits on the mutex.
	time.Sleep(20 * time.Millisecond)
	ctxB, cancelB := context.WithCancel(context.Background())
	resB := make(chan error, 1)
	go func() {
		_, err := l.Embed(ctxB, []string{"b"})
		resB <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancelB()
	close(release)

	if err := <-resA; err == nil || !strings.Contains(err.Error(), "load failed") {
		t.Fatalf("A must see the load error, got %v", err)
	}
	if err := <-resB; !errors.Is(err, context.Canceled) {
		t.Fatalf("B, canceled while queued, must surface cancellation instead of retrying the load, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if opens != 1 {
		t.Fatalf("canceled queued request must not trigger another Open, got %d opens", opens)
	}
}
