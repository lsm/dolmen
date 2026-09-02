package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

type Provider interface {
	Name() string
	ModelName() string
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

type None struct{}

func (None) Name() string { return "none" }

func (None) ModelName() string { return "" }

func (None) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return nil, fmt.Errorf("no embedding provider configured: set DOLMEN_EMBED_PROVIDER=openai plus DOLMEN_EMBED_API_KEY (or OPENAI_API_KEY); optionally DOLMEN_EMBED_BASE_URL and DOLMEN_EMBED_MODEL")
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

func (o *OpenAI) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if o.APIKey == "" {
		return nil, fmt.Errorf("embedding API key missing: set DOLMEN_EMBED_API_KEY or OPENAI_API_KEY")
	}
	client := o.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	out := make([][]float32, len(texts))
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
		req.Header.Set("Authorization", "Bearer "+o.APIKey)
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
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
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
		sort.SliceStable(decoded.Data, func(i, j int) bool { return decoded.Data[i].Index < decoded.Data[j].Index })
		for i, d := range decoded.Data {
			out[start+i] = d.Embedding
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

func FromEnv() Provider {
	switch strings.ToLower(os.Getenv("DOLMEN_EMBED_PROVIDER")) {
	case "", "none":
		return None{}
	case "openai":
		return &OpenAI{
			BaseURL: envOr("DOLMEN_EMBED_BASE_URL", "https://api.openai.com/v1"),
			Model:   envOr("DOLMEN_EMBED_MODEL", "text-embedding-3-small"),
			APIKey:  envOr("DOLMEN_EMBED_API_KEY", os.Getenv("OPENAI_API_KEY")),
		}
	default:
		return None{}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
