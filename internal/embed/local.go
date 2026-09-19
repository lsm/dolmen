package embed

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/rostamlabs/rembed"
)

const localWorkers = 2

const localModelDir = "models"

var localModelIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9._-]+$`)

const CacheManifestName = ".dolmen-sizes.json"

type LocalEngine interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

type LoadError struct {
	Model string
	Err   error
}

func (e *LoadError) Error() string {
	return fmt.Sprintf("load local embedding model %s (first use downloads it from the Hugging Face Hub into the model cache; pre-seed the cache or pass a model directory for offline installs): %v", e.Model, e.Err)
}

func (e *LoadError) Unwrap() error { return e.Err }

func (e *LoadError) IsHubID() bool { return localModelIDRe.MatchString(e.Model) }

func (e *LoadError) CacheDirName() string { return modelCacheDirName(e.Model) }

type Local struct {
	Model string

	CacheRoot string

	Open func() (LocalEngine, error)

	mu  sync.Mutex
	eng LocalEngine
}

func (l *Local) Name() string { return "local" }

func (l *Local) ModelName() string { return l.Model }

func (l *Local) Identity() string {
	if identityLegacy(l.Model) {
		return "local/" + l.Model
	}
	return "local/v2:" + escapeIdentityReference(l.Model) + identityMarker(l.Model)
}

func (l *Local) HubModel() bool { return localModelIDRe.MatchString(l.Model) }

func (l *Local) Cached() bool {
	if l.Open != nil {
		return true
	}
	if l.HubModel() {
		return seededCacheDir(l.CacheRoot, l.Model) != ""
	}

	if filepath.IsAbs(l.Model) {
		return completeModelDir(l.Model)
	}
	return false
}

func modelCacheDirName(model string) string { return strings.ReplaceAll(model, "/", "--") }

func (l *Local) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	_, passage := e5Prefixes(l.Model)
	return l.embed(ctx, texts, passage)
}

func (l *Local) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	query, _ := e5Prefixes(l.Model)
	vecs, err := l.embed(ctx, []string{text}, query)
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("local embedding model %s: %d vectors for one query text", l.Model, len(vecs))
	}
	return vecs[0], nil
}

func (l *Local) embed(ctx context.Context, texts []string, prefix string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	eng, err := l.engine(ctx)
	if err != nil {
		return nil, err
	}
	texts = prefixAll(prefix, texts)
	vecs, err := eng.Embed(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("local embedding model %s: %w", l.Model, err)
	}
	return vecs, nil
}

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
		if cacheRef := seededCacheDir(l.CacheRoot, l.Model); cacheRef != "" {
			ref = cacheRef
		}
		open = func() (LocalEngine, error) {
			return rembed.Load(ref, rembed.WithInt8(), rembed.WithWorkers(localWorkers))
		}
	}
	eng, err := open()
	if err != nil {
		return nil, &LoadError{Model: l.Model, Err: err}
	}
	l.eng = eng
	return eng, nil
}

func localRef(model string) string {
	if localModelIDRe.MatchString(model) {
		return "hf:" + model
	}
	return model
}

func seededCacheDir(cacheRoot, model string) string {
	if cacheRoot == "" || !localModelIDRe.MatchString(model) {
		return ""
	}
	dir := filepath.Join(cacheRoot, modelCacheDirName(model))
	if !completeModelDir(dir) {
		return ""
	}
	return dir
}

func fileNonEmpty(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir() && fi.Size() > 0
}

func completeModelDir(dir string) bool {
	for _, f := range []string{"config.json", "tokenizer_config.json", "modules.json"} {
		if !fileNonEmpty(filepath.Join(dir, f)) {
			return false
		}
	}

	var hf struct {
		ModelType string `json:"model_type"`
	}
	cfgRaw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return false
	}
	if err := json.Unmarshal(cfgRaw, &hf); err != nil {
		return false
	}
	var tc struct {
		TokenizerClass string `json:"tokenizer_class"`
	}
	tcRaw, err := os.ReadFile(filepath.Join(dir, "tokenizer_config.json"))
	if err != nil {
		return false
	}
	if err := json.Unmarshal(tcRaw, &tc); err != nil {
		return false
	}
	tokFiles, probe := TokenizerFiles(hf.ModelType, tc.TokenizerClass)
	if probe {
		if fileNonEmpty(filepath.Join(dir, "sentencepiece.bpe.model")) {
			tokFiles = nil
		}
	}
	for _, f := range tokFiles {
		if !fileNonEmpty(filepath.Join(dir, f)) {
			return false
		}
	}

	if !moduleArtifactsComplete(dir) {
		return false
	}

	if _, err := os.Stat(filepath.Join(dir, CacheManifestName)); err == nil {
		manifestRaw, err := os.ReadFile(filepath.Join(dir, CacheManifestName))
		if err != nil {
			return false
		}
		var sizes map[string]int64
		if err := json.Unmarshal(manifestRaw, &sizes); err != nil {
			return false
		}
		for name, want := range sizes {
			if !validCacheRel(name) {
				return false
			}
			fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name)))
			if err != nil || fi.IsDir() || fi.Size() != want {
				return false
			}
		}
	}

	single := filepath.Join(dir, "model.safetensors")
	if fileNonEmpty(single) {
		return true
	}

	idx := filepath.Join(dir, "model.safetensors.index.json")
	if !fileNonEmpty(idx) {
		return false
	}
	idxRaw, err := os.ReadFile(idx)
	if err != nil {
		return false
	}
	var sharded struct {
		WeightMap map[string]string `json:"weight_map"`
	}
	if err := json.Unmarshal(idxRaw, &sharded); err != nil {
		return false
	}
	if len(sharded.WeightMap) == 0 {
		return false
	}
	seen := make(map[string]struct{})
	for _, shard := range sharded.WeightMap {
		if !validCacheShard(shard) || !strings.HasSuffix(shard, ".safetensors") {
			return false
		}
		if _, ok := seen[shard]; ok {
			continue
		}
		seen[shard] = struct{}{}
		if !fileNonEmpty(filepath.Join(dir, shard)) {
			return false
		}
	}
	return true
}

func validCacheShard(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, `/\`)
}

func validCacheRel(name string) bool {
	if name == "" || strings.Contains(name, "..") || strings.ContainsRune(name, '\\') {
		return false
	}
	return !filepath.IsAbs(name) && name == path.Clean(name)
}

func TokenizerFiles(modelType, tokenizerClass string) (files []string, probe bool) {
	if modelType == "xlm-roberta" || strings.HasPrefix(tokenizerClass, "XLMRobertaTokenizer") {
		return []string{"sentencepiece.bpe.model"}, false
	}
	if modelType == "roberta" {
		return []string{"vocab.json", "merges.txt"}, true
	}
	if modelType == "modernbert" || modelType == "qwen3" {
		return []string{"tokenizer.json"}, false
	}
	if modelType == "gemma3_text" || modelType == "gemma3" {
		return []string{"tokenizer.json"}, false
	}
	return []string{"vocab.txt"}, true
}

func moduleArtifactsComplete(dir string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, "modules.json"))
	if err != nil {
		return false
	}
	var modules []struct {
		Path string `json:"path"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &modules); err != nil {
		return false
	}
	seen := make(map[string]struct{})
	for _, m := range modules {

		if m.Path == "" || !validCacheShard(m.Path) || parameterlessModule(m.Type) {
			continue
		}
		if _, ok := seen[m.Path]; ok {
			continue
		}
		seen[m.Path] = struct{}{}

		sub := filepath.Join(dir, m.Path)
		if !fileNonEmpty(filepath.Join(sub, "config.json")) {
			return false
		}
		if strings.HasSuffix(m.Type, ".Dense") {
			if !fileNonEmpty(filepath.Join(sub, "model.safetensors")) {
				return false
			}
		}
	}
	return true
}

func parameterlessModule(moduleType string) bool {
	for _, suffix := range []string{".Normalize", ".LayerNorm"} {
		if strings.HasSuffix(moduleType, suffix) {
			return true
		}
	}
	return false
}

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

func localCacheRoot(dataDir string) string {
	if v := os.Getenv("REMBED_CACHE"); v != "" {
		return v
	}
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, localModelDir)
}

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
