package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lsm/dolmen/internal/embed"
)

var (
	errNotFound = errors.New("file not found")

	modelIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9._-]+$`)
)

var hubBase = "https://huggingface.co"

func main() {
	model := flag.String("model", "sentence-transformers/all-MiniLM-L6-v2", "Hugging Face model id to package")
	revision := flag.String("revision", "main", "model revision to download (commit, tag, or branch)")
	out := flag.String("out", "", "output tarball path")
	flag.Parse()

	if *out == "" {
		log.Fatal("-out is required")
	}
	if !modelIDRe.MatchString(*model) {
		log.Fatalf("invalid model id %q", *model)
	}
	if strings.Contains(*model, "..") {
		log.Fatalf("invalid model id %q", *model)
	}

	cache, err := os.MkdirTemp("", "pack-model-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(cache)

	modelDir := filepath.Join(cache, strings.ReplaceAll(*model, "/", "--"))
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		log.Fatal(err)
	}

	if err := ensure(*model, *revision, modelDir); err != nil {
		log.Fatalf("download %s: %v", *model, err)
	}

	if err := writeTar(modelDir, *model, *out); err != nil {
		log.Fatalf("package %s: %v", *out, err)
	}

	if fi, err := os.Stat(*out); err == nil {
		log.Printf("packaged %s from %s (%.1f MB)", *out, *model, float64(fi.Size())/(1<<20))
	} else {
		log.Printf("packaged %s", *out)
	}
}

func ensure(modelID, revision, dir string) error {
	fetched := []string{}
	cleanup := func(files ...string) {
		for _, f := range files {
			_ = os.Remove(filepath.Join(dir, filepath.FromSlash(f)))
		}
		for _, sub := range []string{"1_Pooling", "2_Dense", "3_Dense"} {
			_ = os.Remove(filepath.Join(dir, sub))
		}
	}

	if err := fetch(modelID, revision, "config.json", dir); err != nil {
		return err
	}
	fetched = append(fetched, "config.json")

	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return fmt.Errorf("read config.json: %w", err)
	}
	var hf struct {
		ModelType string `json:"model_type"`
	}
	if err := json.Unmarshal(raw, &hf); err != nil {
		return fmt.Errorf("parse config.json: %w", err)
	}
	if !supported(hf.ModelType) {
		return fmt.Errorf("model_type %q is not supported (rembed supports bert, distilbert, modernbert, qwen3, gemma3, roberta, xlm-roberta, and mpnet)", hf.ModelType)
	}

	if err := fetch(modelID, revision, "tokenizer_config.json", dir); err != nil {
		cleanup(fetched...)
		return err
	}
	fetched = append(fetched, "tokenizer_config.json")

	raw, err = os.ReadFile(filepath.Join(dir, "tokenizer_config.json"))
	if err != nil {
		cleanup(fetched...)
		return fmt.Errorf("read tokenizer_config.json: %w", err)
	}
	var tc struct {
		TokenizerClass string `json:"tokenizer_class"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		cleanup(fetched...)
		return fmt.Errorf("parse tokenizer_config.json: %w", err)
	}

	tokFiles, probe := embed.TokenizerFiles(hf.ModelType, tc.TokenizerClass)
	if probe {
		err := fetch(modelID, revision, "sentencepiece.bpe.model", dir)
		switch {
		case err == nil:
			tokFiles = nil
			fetched = append(fetched, "sentencepiece.bpe.model")
		case errors.Is(err, errNotFound):

		default:
			cleanup(fetched...)
			return err
		}
	}

	for _, f := range append(tokFiles, "1_Pooling/config.json", "modules.json") {
		if err := fetch(modelID, revision, f, dir); err != nil {
			cleanup(fetched...)
			return err
		}
		fetched = append(fetched, f)
	}

	if hf.ModelType == "gemma3_text" || hf.ModelType == "gemma3" {
		for _, f := range []string{
			"2_Dense/config.json", "2_Dense/model.safetensors",
			"3_Dense/config.json", "3_Dense/model.safetensors",
		} {
			if err := fetch(modelID, revision, f, dir); err != nil {
				cleanup(fetched...)
				return err
			}
			fetched = append(fetched, f)
		}
	}

	weightFiles, err := fetchWeights(modelID, revision, dir)
	if err != nil {
		cleanup(fetched...)
		return err
	}
	fetched = append(fetched, weightFiles...)
	return nil
}

func supported(modelType string) bool {
	switch modelType {
	case "bert", "distilbert", "modernbert", "qwen3", "gemma3_text", "gemma3", "roberta", "xlm-roberta", "mpnet":
		return true
	}
	return false
}

func fetch(modelID, revision, name, dir string) error {
	dst := filepath.Join(dir, filepath.FromSlash(name))
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	var wantSHA string
	var wantSize int64 = -1
	client := &http.Client{
		Timeout: 15 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {

			if r := req.Response; r != nil {
				if etag := strings.Trim(r.Header.Get("X-Linked-Etag"), `W/"`); len(etag) == 64 {
					wantSHA = etag
				}
				if v := r.Header.Get("X-Linked-Size"); v != "" {
					_, _ = fmt.Sscan(v, &wantSize)
				}
			}
			return nil
		},
	}

	url := fmt.Sprintf("%s/%s/resolve/%s/%s", hubBase, modelID, revision, name)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "dolmen-pack-model/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch %s/%s: %w", modelID, name, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return fmt.Errorf("fetch %s/%s: %w", modelID, name, errNotFound)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("fetch %s/%s: HTTP %s (gated, private, or missing)", modelID, name, resp.Status)
	default:
		return fmt.Errorf("fetch %s/%s: HTTP %s", modelID, name, resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".pack-dl-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	hasher := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hasher), resp.Body)
	if err == nil {
		err = tmp.Close()
	} else {
		_ = tmp.Close()
	}
	if err != nil {
		return fmt.Errorf("download %s/%s: %w", modelID, name, err)
	}
	if wantSize > 0 && n != wantSize {
		return fmt.Errorf("download %s/%s: got %d bytes, want %d", modelID, name, n, wantSize)
	}
	if resp.ContentLength > 0 && n != resp.ContentLength {
		return fmt.Errorf("download %s/%s: got %d bytes, want %d", modelID, name, n, resp.ContentLength)
	}
	if wantSHA != "" {
		got := hex.EncodeToString(hasher.Sum(nil))
		if got != wantSHA {
			return fmt.Errorf("download %s/%s: sha256 mismatch (got %s, want %s)", modelID, name, got, wantSHA)
		}
	}
	return os.Rename(tmp.Name(), dst)
}

func fetchWeights(modelID, revision, dir string) ([]string, error) {
	err := fetch(modelID, revision, "model.safetensors", dir)
	if err == nil {
		return []string{"model.safetensors"}, nil
	}
	if !errors.Is(err, errNotFound) {
		return nil, err
	}

	if err := fetch(modelID, revision, "model.safetensors.index.json", dir); err != nil {
		return nil, fmt.Errorf("%s has no model.safetensors or shard index: %w", modelID, err)
	}
	got := []string{"model.safetensors.index.json"}

	raw, err := os.ReadFile(filepath.Join(dir, "model.safetensors.index.json"))
	if err != nil {
		return got, err
	}
	var idx struct {
		WeightMap map[string]string `json:"weight_map"`
	}
	if err := json.Unmarshal(raw, &idx); err != nil {
		return got, fmt.Errorf("bad shard index for %s: %w", modelID, err)
	}

	seen := make(map[string]struct{})
	shards := make([]string, 0, len(idx.WeightMap))
	for _, f := range idx.WeightMap {
		if !validShardName(f) {
			return got, fmt.Errorf("%s: unsafe shard name %q", modelID, f)
		}
		if _, ok := seen[f]; ok {
			continue
		}
		seen[f] = struct{}{}
		shards = append(shards, f)
	}
	sort.Strings(shards)

	for _, shard := range shards {
		if err := fetch(modelID, revision, shard, dir); err != nil {
			return got, err
		}
		got = append(got, shard)
	}
	return got, nil
}

func validShardName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, `/\`)
}

func writeTar(modelDir, modelID, outPath string) (err error) {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}

	f, err := os.Create(outPath)
	if err != nil {
		return err
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	defer func() {
		if cerr := tw.Close(); cerr != nil && err == nil {
			err = cerr
		}
		if cerr := gw.Close(); cerr != nil && err == nil {
			err = cerr
		}
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	prefix := strings.ReplaceAll(modelID, "/", "--")

	archiveEpoch := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	writeEntry := func(name string, size int64, body io.Reader) error {
		if err := tw.WriteHeader(&tar.Header{
			Name:     path.Join(prefix, filepath.ToSlash(name)),
			Size:     size,
			Mode:     0o644,
			ModTime:  archiveEpoch,
			Typeflag: tar.TypeReg,
		}); err != nil {
			return err
		}
		_, err := io.Copy(tw, body)
		return err
	}

	type entry struct {
		rel string
		fi  os.FileInfo
	}
	var entries []entry
	walkErr := filepath.Walk(modelDir, func(file string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(modelDir, file)
		if err != nil {
			return err
		}
		entries = append(entries, entry{rel: rel, fi: fi})
		return nil
	})
	if walkErr != nil {
		return walkErr
	}

	sizes := make(map[string]int64, len(entries))
	for _, e := range entries {
		sizes[filepath.ToSlash(e.rel)] = e.fi.Size()
	}
	manifestRaw, err := json.Marshal(sizes)
	if err != nil {
		return err
	}
	if err := writeEntry(embed.CacheManifestName, int64(len(manifestRaw)), bytes.NewReader(manifestRaw)); err != nil {
		return err
	}

	for _, e := range entries {
		in, err := os.Open(filepath.Join(modelDir, e.rel))
		if err != nil {
			return err
		}
		werr := writeEntry(e.rel, e.fi.Size(), in)
		in.Close()
		if werr != nil {
			return werr
		}
	}
	return nil
}
