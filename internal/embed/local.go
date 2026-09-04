package embed

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/rostamlabs/rembed"
)

// localWorkers caps the CPU workers one embedding call may use (rembed's
// default is every core plus a spinning fork-join pool). Embedding must not
// starve the API server, so the cap stays small; WithWorkers(1) would be
// fully serial, 2 keeps a little batch parallelism for migrate backfills.
const localWorkers = 2

// localModelDir is the subdirectory of the data dir where rembed caches
// downloaded model weights (one "org--name" directory per model).
const localModelDir = "models"

// localModelIDRe matches a Hugging Face model id (org/name) — the same
// shape rembed's hub accepts, so DOLMEN_EMBED_MODEL validates locally
// before any download is attempted.
var localModelIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9._-]+$`)

// LocalEngine is the slice of rembed's *Embedder the provider needs —
// exported so tests outside the package can inject a stub engine, and the
// seam that keeps the inference engine swappable.
type LocalEngine interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Local embeds in-process via rembed — pure Go inference, no cgo, no ONNX
// Runtime — so vectorize works with zero external endpoints. Weights are
// never in the binary: the model downloads from the Hugging Face Hub on
// first use (HF_TOKEN honored for gated repos) into the model cache under
// the data dir, and every later load reuses it.
//
// The model loads lazily on the first Embed call, so startup stays instant
// and a server without network only fails when embedding is requested.
// Concurrent Embed calls are safe: the load happens once under a mutex and
// rembed's Embedder is safe for concurrent use.
type Local struct {
	// Model is a Hugging Face model id (org/name) or an absolute path to a
	// model directory (a copy of the cache's org--name dir, or any HF repo
	// checkout) for offline installs.
	Model string

	// CacheRoot is the directory that holds downloaded model caches. It is
	// set by NewProvider from REMBED_CACHE or <data>/models.
	CacheRoot string

	// Open loads the engine; overridable in tests. When nil it loads Model
	// (via localRef) with weight-only int8 and the worker cap above.
	Open func() (LocalEngine, error)

	mu  sync.Mutex
	eng LocalEngine
}

func (l *Local) Name() string { return "local" }

func (l *Local) ModelName() string { return l.Model }

// Identity pins tables to this provider and model: "local/<model>". A model
// change (or a switch to/from the OpenAI provider) is a different identity,
// so inserts and text searches are rejected until the table is re-embedded
// via migrate — exactly as with the OpenAI provider.
func (l *Local) Identity() string { return "local/" + l.Model }

// Cached reports whether the model weights are already on disk. A test stub
// (Open != nil) is treated as cached so tests do not trigger the warning.
func (l *Local) Cached() bool {
	if l.Open != nil {
		return true
	}
	if localModelIDRe.MatchString(l.Model) {
		if l.CacheRoot == "" {
			return false
		}
		// rembed stores Hub models as an org--name directory with the
		// weights in model.safetensors.
		dir := filepath.Join(l.CacheRoot, modelCacheDirName(l.Model))
		if fi, err := os.Stat(filepath.Join(dir, "model.safetensors")); err == nil && !fi.IsDir() {
			return true
		}
		return false
	}
	// An absolute model-directory path is its own cache.
	if filepath.IsAbs(l.Model) {
		if fi, err := os.Stat(l.Model); err == nil && fi.IsDir() {
			return true
		}
	}
	return false
}

// modelCacheDirName returns the on-disk directory name for a Hugging Face
// model id, matching the org--name layout rembed uses for the cache.
func modelCacheDirName(model string) string { return strings.ReplaceAll(model, "/", "--") }

func (l *Local) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	eng, err := l.engine(ctx)
	if err != nil {
		return nil, err
	}
	vecs, err := eng.Embed(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("local embedding model %s: %w", l.Model, err)
	}
	return vecs, nil
}

// engine returns the loaded engine, loading it on first use. A failed load
// is not memoized: a transient download failure must not disable embedding
// until restart, so the next Embed call tries again. ctx is rechecked after
// the lock is acquired — a request canceled while queued behind a failed
// load must not start another download its client no longer wants.
func (l *Local) engine(ctx context.Context) (LocalEngine, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.eng != nil {
		return l.eng, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	open := l.Open
	if open == nil {
		ref := localRef(l.Model)
		open = func() (LocalEngine, error) {
			return rembed.Load(ref, rembed.WithInt8(), rembed.WithWorkers(localWorkers))
		}
	}
	eng, err := open()
	if err != nil {
		return nil, fmt.Errorf("load local embedding model %s (first use downloads it from the Hugging Face Hub into the model cache; pre-seed the cache or pass a model directory for offline installs): %w", l.Model, err)
	}
	l.eng = eng
	return eng, nil
}

// localRef maps a model to the ref rembed.Load takes: a Hub id gets the
// "hf:" prefix so loading always means the Hub, never a same-named
// directory in the working directory; anything else is a model-directory
// path, which rembed loads as-is.
func localRef(model string) string {
	if localModelIDRe.MatchString(model) {
		return "hf:" + model
	}
	return model
}

// validateLocalModel accepts a Hugging Face model id (org/name) or an
// existing absolute model-directory path, and rejects everything else —
// including relative directory paths, whose identity would depend on the
// working directory the server happens to start in.
func validateLocalModel(model string) error {
	if strings.Contains(model, "..") {
		return fmt.Errorf("DOLMEN_EMBED_MODEL %q must not contain \"..\"", model)
	}
	if localModelIDRe.MatchString(model) {
		return nil
	}
	if filepath.IsAbs(model) {
		if fi, err := os.Stat(model); err == nil && fi.IsDir() {
			return nil
		}
		return fmt.Errorf("DOLMEN_EMBED_MODEL %q is not an existing model directory", model)
	}
	return fmt.Errorf("DOLMEN_EMBED_MODEL %q is neither a Hugging Face model id (org/name) nor an absolute model-directory path", model)
}

// localCacheRoot returns the model cache directory for the given data dir.
// An explicit REMBED_CACHE wins, so operators can share one cache across
// instances; otherwise the cache lands under <data>/models.
func localCacheRoot(dataDir string) string {
	if v := os.Getenv("REMBED_CACHE"); v != "" {
		return v
	}
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, localModelDir)
}

// useLocalCache points rembed's model cache at the data dir. An explicit
// REMBED_CACHE wins, so operators can share one cache across instances;
// otherwise the cache lands under <data>/models, pre-created here (0700,
// like the data dir itself) so an unwritable data dir fails at startup
// rather than at the first embedding call.
func useLocalCache(dataDir string) error {
	if os.Getenv("REMBED_CACHE") != "" {
		return nil
	}
	if dataDir == "" {
		return nil
	}
	dir := filepath.Join(dataDir, localModelDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create model cache %s: %w", dir, err)
	}
	return os.Setenv("REMBED_CACHE", dir)
}
