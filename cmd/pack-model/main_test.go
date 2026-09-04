package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackageDefaultModelLayout(t *testing.T) {
	files := map[string][]byte{
		"config.json": []byte(`{"model_type": "bert", "vocab_size": 30522, "hidden_size": 384, "num_hidden_layers": 6, "num_attention_heads": 12, "intermediate_size": 1536, "max_position_embeddings": 512}`),
		"tokenizer_config.json": []byte(`{"do_lower_case": true, "cls_token": "[CLS]", "sep_token": "[SEP]", "unk_token": "[UNK]", "tokenizer_class": "BertTokenizer"}`),
		"vocab.txt":             []byte("[PAD]\n[UNK]\n[CLS]\n"),
		"1_Pooling/config.json": []byte(`{"pooling_mode_cls_token": true}`),
		"modules.json":          []byte(`[{"type": "sentence_transformers.models.Transformer"}, {"type": "sentence_transformers.models.Pooling"}]`),
		"model.safetensors":     []byte("fake safetensors weights"),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Expect /repo/resolve/<rev>/<file>
		if !strings.Contains(r.URL.Path, "/resolve/pinned/") {
			http.NotFound(w, r)
			return
		}
		file := strings.SplitN(r.URL.Path, "/resolve/pinned/", 2)[1]
		data, ok := files[file]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(data)
	}))
	defer srv.Close()

	old := hubBase
	hubBase = srv.URL + "/repo"
	defer func() { hubBase = old }()

	dir := t.TempDir()
	outDir := t.TempDir()
	out := filepath.Join(outDir, "model.tar.gz")

	if err := ensure("test-org/all-MiniLM-L6-v2", "pinned", dir); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := writeTar(dir, "test-org/all-MiniLM-L6-v2", out); err != nil {
		t.Fatalf("writeTar: %v", err)
	}

	f, err := os.Open(out)
	if err != nil {
		t.Fatalf("open tar: %v", err)
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)

	found := make(map[string]int64)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		found[hdr.Name] = hdr.Size
		// Sanity: files should be non-empty.
		if hdr.Size == 0 {
			t.Fatalf("empty file in tar: %s", hdr.Name)
		}
	}

	prefix := "test-org--all-MiniLM-L6-v2/"
	for name := range files {
		want := prefix + name
		if _, ok := found[want]; !ok {
			t.Fatalf("missing tar entry: %s", want)
		}
	}
	if len(found) != len(files) {
		t.Fatalf("unexpected tar entries: %v", found)
	}
}

func TestPackageShardedWeights(t *testing.T) {
	shards := map[string][]byte{
		"model-00001-of-00002.safetensors": []byte("shard one"),
		"model-00002-of-00002.safetensors": []byte("shard two"),
	}
	files := map[string][]byte{
		"config.json": []byte(`{"model_type": "bert"}`),
		"tokenizer_config.json": []byte(`{"tokenizer_class": "BertTokenizer"}`),
		"vocab.txt":             []byte("a\nb\n"),
		"1_Pooling/config.json": []byte(`{"pooling_mode_cls_token": true}`),
		"modules.json":          []byte(`[]`),
		"model.safetensors.index.json": []byte(`{"weight_map": {"layer.0": "model-00001-of-00002.safetensors", "layer.1": "model-00002-of-00002.safetensors"}}`),
	}
	for name, data := range shards {
		files[name] = data
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/resolve/pinned/") {
			http.NotFound(w, r)
			return
		}
		file := strings.SplitN(r.URL.Path, "/resolve/pinned/", 2)[1]
		data, ok := files[file]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
	}))
	defer srv.Close()

	old := hubBase
	hubBase = srv.URL + "/repo"
	defer func() { hubBase = old }()

	dir := t.TempDir()
	outDir := t.TempDir()
	out := filepath.Join(outDir, "sharded.tar.gz")

	if err := ensure("org/sharded", "pinned", dir); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if err := writeTar(dir, "org/sharded", out); err != nil {
		t.Fatalf("writeTar: %v", err)
	}

	f, err := os.Open(out)
	if err != nil {
		t.Fatalf("open tar: %v", err)
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)

	count := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		if !strings.HasPrefix(hdr.Name, "org--sharded/") {
			t.Fatalf("unexpected tar entry: %s", hdr.Name)
		}
		count++
	}
	if count != len(files) {
		t.Fatalf("expected %d tar entries, got %d", len(files), count)
	}
}

func TestPackageUnsupportedModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/resolve/pinned/") {
			http.NotFound(w, r)
			return
		}
		file := strings.SplitN(r.URL.Path, "/resolve/pinned/", 2)[1]
		if file == "config.json" {
			w.Write([]byte(`{"model_type": "unsupported"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	old := hubBase
	hubBase = srv.URL + "/repo"
	defer func() { hubBase = old }()

	dir := t.TempDir()
	if err := ensure("org/bad", "pinned", dir); err == nil {
		t.Fatal("expected error for unsupported model type")
	} else if !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidShardName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"model-00001-of-00002.safetensors", true},
		{"model.safetensors", true},
		{"", false},
		{".", false},
		{"..", false},
		{"../../etc/passwd", false},
		{"/tmp/file", false},
	}
	for _, tc := range cases {
		if got := validShardName(tc.name); got != tc.ok {
			t.Errorf("validShardName(%q) = %v, want %v", tc.name, got, tc.ok)
		}
	}
}

func TestFetchChecksXLinkedEtag(t *testing.T) {
	data := []byte("hello weights")
	want := fmt.Sprintf("%x", sha256.Sum256(data))

	var base string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cdn/model.safetensors" {
			w.Write(data)
			return
		}
		// First redirect: simulate the HF 302 to the CDN.
		w.Header().Set("X-Linked-Etag", `"`+want+`"`)
		w.Header().Set("X-Linked-Size", fmt.Sprintf("%d", len(data)))
		w.Header().Set("Location", base+"/cdn/model.safetensors")
		w.WriteHeader(http.StatusFound)
	})
	srv := httptest.NewServer(handler)
	base = srv.URL
	defer srv.Close()

	old := hubBase
	hubBase = srv.URL
	defer func() { hubBase = old }()

	dir := t.TempDir()
	if err := fetch("org/model", "rev", "model.safetensors", dir); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "model.safetensors"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(b, data) {
		t.Fatalf("got %q, want %q", b, data)
	}
}
