package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

type Provider interface {
	Name() string
	Identity() string
	// ModelName reports the configured model name — status surface for
	// describe_server; empty when the provider has no model to name.
	ModelName() string
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

type None struct{}

func (None) Name() string { return "none" }

func (None) Identity() string { return "" }

func (None) ModelName() string { return "" }

func (None) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return nil, fmt.Errorf("no embedding provider configured: set DOLMEN_EMBED_PROVIDER=local for in-process embeddings (no external service), or DOLMEN_EMBED_PROVIDER=openai plus DOLMEN_EMBED_API_KEY (or OPENAI_API_KEY) for an external endpoint; optionally DOLMEN_EMBED_BASE_URL and DOLMEN_EMBED_MODEL")
}

type OpenAI struct {
	BaseURL string
	Model   string
	APIKey  string
	Client  *http.Client
}

const openAIBatch = 96

func (o *OpenAI) Name() string { return "openai" }

func (o *OpenAI) ModelName() string { return o.Model }

func (o *OpenAI) Identity() string {
	return o.Name() + "|" + strings.TrimRight(o.BaseURL, "/") + "|" + o.Model
}

func (o *OpenAI) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	client := o.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	out := make([][]float32, len(texts))
	dim := 0
	for start := 0; start < len(texts); start += openAIBatch {
		end := start + openAIBatch
		if end > len(texts) {
			end = len(texts)
		}
		batch := texts[start:end]
		body, err := json.Marshal(map[string]any{"model": o.Model, "input": batch})
		if err != nil {
			return nil, err
		}
		url := strings.TrimRight(o.BaseURL, "/") + "/embeddings"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if o.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+o.APIKey)
		}
		res, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		raw, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
		res.Body.Close()
		if err != nil {
			return nil, err
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("embeddings API returned %d: %s", res.StatusCode, truncate(string(raw), 500))
		}
		var decoded struct {
			Data []struct {
				Index     *int       `json:"index"`
				Embedding []*float64 `json:"embedding"`
			} `json:"data"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return nil, err
		}
		if decoded.Error != nil {
			return nil, fmt.Errorf("embeddings API error: %s", decoded.Error.Message)
		}
		if len(decoded.Data) != len(batch) {
			return nil, fmt.Errorf("embeddings API returned %d vectors for %d texts", len(decoded.Data), len(batch))
		}
		for i := range decoded.Data {
			if decoded.Data[i].Index == nil {
				return nil, fmt.Errorf("embeddings API returned an embedding without an index (position %d)", i)
			}
		}
		sort.SliceStable(decoded.Data, func(i, j int) bool { return *decoded.Data[i].Index < *decoded.Data[j].Index })
		for i, d := range decoded.Data {
			if *d.Index != i {
				return nil, fmt.Errorf("embeddings API returned inconsistent indices (expected 0..%d)", len(batch)-1)
			}
			if len(d.Embedding) == 0 {
				return nil, fmt.Errorf("embeddings API returned an empty embedding for input %d", start+i)
			}
			if dim == 0 {
				dim = len(d.Embedding)
			} else if len(d.Embedding) != dim {
				return nil, fmt.Errorf("embeddings API returned a %d-dimensional vector for input %d after a %d-dimensional one", len(d.Embedding), start+i, dim)
			}
			vec := make([]float32, len(d.Embedding))
			for j, e := range d.Embedding {
				if e == nil {
					return nil, fmt.Errorf("embeddings API returned a null entry in the embedding for input %d", start+i)
				}
				if *e > math.MaxFloat32 || *e < -math.MaxFloat32 {
					return nil, fmt.Errorf("embeddings API returned a value outside the float32 range for input %d", start+i)
				}
				vec[j] = float32(*e)
			}
			out[start+i] = vec
		}
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// NewProvider returns an embedding provider for the given configuration.
// Valid providers are "none" (or ""), "local" (in-process inference via
// rembed; dataDir is where the model cache lands), and "openai" (any
// OpenAI-compatible endpoint).
func NewProvider(provider, baseURL, model, apiKey, dataDir string) (Provider, error) {
	switch strings.ToLower(provider) {
	case "", "none":
		return None{}, nil
	case "local":
		if model == "" {
			model = "sentence-transformers/all-MiniLM-L6-v2"
		}
		if err := validateLocalModel(model); err != nil {
			return nil, err
		}
		if err := useLocalCache(dataDir); err != nil {
			return nil, err
		}
		return &Local{Model: model}, nil
	case "openai":
		if baseURL == "" {
			baseURL = "https://api.openai.com/v1"
		}
		if model == "" {
			model = "text-embedding-3-small"
		}
		return &OpenAI{BaseURL: baseURL, Model: model, APIKey: apiKey}, nil
	default:
		return nil, fmt.Errorf("unknown embedding provider %q (valid: none, local, openai)", provider)
	}
}
