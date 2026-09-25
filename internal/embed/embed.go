package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type traceContextKey struct{}

func WithTraceContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, traceContextKey{}, true)
}

const usageInputTokensKey = attribute.Key("gen_ai.usage.input_tokens")

type Provider interface {
	Name() string
	Identity() string

	ModelName() string

	Embed(ctx context.Context, texts []string) ([][]float32, error)

	EmbedQuery(ctx context.Context, text string) ([]float32, error)
}

var e5NameRe = regexp.MustCompile(`(?i)(?:^|[-_])e5(?:[-_]|$)`)

func e5Prefixes(model string) (query, passage string) {
	name := filepath.Base(model)
	if !e5NameRe.MatchString(name) || strings.Contains(strings.ToLower(name), "instruct") {
		return "", ""
	}
	return "query: ", "passage: "
}

func identityMarker(model string) string {
	if query, _ := e5Prefixes(model); query != "" {
		return "#e5"
	}
	return ""
}

func escapeIdentityReference(model string) string {
	return strings.NewReplacer("%", "%25", "#", "%23").Replace(model)
}

func identityLegacy(model string) bool {
	return identityMarker(model) == "" && !strings.ContainsAny(model, "%#")
}

func prefixAll(prefix string, texts []string) []string {
	if prefix == "" {
		return texts
	}
	out := make([]string, len(texts))
	for i, t := range texts {
		out[i] = prefix + t
	}
	return out
}

type None struct{}

func (None) Name() string { return "none" }

func (None) Identity() string { return "" }

func (None) ModelName() string { return "" }

func (None) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return nil, errNoProvider
}

func (None) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	return nil, errNoProvider
}

var errNoProvider = fmt.Errorf("no embedding provider configured: set DOLMEN_EMBED_PROVIDER=local for in-process embeddings (no external service), or DOLMEN_EMBED_PROVIDER=openai plus DOLMEN_EMBED_API_KEY (or OPENAI_API_KEY) for an external endpoint; optionally DOLMEN_EMBED_BASE_URL and DOLMEN_EMBED_MODEL")

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
	provider, model := o.Name(), o.Model
	if !identityLegacy(model) {
		provider += "/v2"
		model = escapeIdentityReference(model) + identityMarker(model)
	}
	trimmed := strings.TrimRight(o.BaseURL, "/")
	u, err := url.Parse(trimmed)
	if err != nil {
		return provider + "|" + trimmed + "|" + model
	}
	u.User = nil
	return provider + "|" + u.String() + "|" + model
}

func (o *OpenAI) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	_, passage := e5Prefixes(o.Model)
	return o.embed(ctx, texts, passage)
}

func (o *OpenAI) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	query, _ := e5Prefixes(o.Model)
	vecs, err := o.embed(ctx, []string{text}, query)
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, fmt.Errorf("embeddings API returned %d vectors for one query text", len(vecs))
	}
	return vecs[0], nil
}

func (o *OpenAI) embed(ctx context.Context, texts []string, prefix string) ([][]float32, error) {
	texts = prefixAll(prefix, texts)
	client := o.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	out := make([][]float32, len(texts))
	dim := 0
	var inputTokens int64
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
		if on, _ := ctx.Value(traceContextKey{}).(bool); on {
			propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(req.Header))
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
			Usage *struct {
				PromptTokens *int64 `json:"prompt_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return nil, err
		}
		if decoded.Error != nil {
			return nil, fmt.Errorf("embeddings API error: %s", decoded.Error.Message)
		}
		if decoded.Usage != nil && decoded.Usage.PromptTokens != nil && inputTokens >= 0 {
			inputTokens += *decoded.Usage.PromptTokens
		} else {
			inputTokens = -1
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
	if span := trace.SpanFromContext(ctx); inputTokens > 0 && span.IsRecording() {
		span.SetAttributes(usageInputTokensKey.Int64(inputTokens))
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

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
		return &Local{Model: model, CacheRoot: localCacheRoot(dataDir)}, nil
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
