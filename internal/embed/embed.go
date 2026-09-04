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
)

type Provider interface {
	Name() string
	Identity() string
	// ModelName reports the configured model name — status surface for
	// describe_server; empty when the provider has no model to name.
	ModelName() string
	// Embed embeds stored-row text (the passage side of the retrieval
	// contract): inserts, updates, and migrate backfills. Symmetric models
	// embed the text as-is; e5-family models get their passage prefix
	// prepended here.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// EmbedQuery embeds search text (the query side). Asymmetric models —
	// the e5 family — need their query prefix here; callers cannot supply
	// it themselves, because dolmen embeds exactly the text the request
	// carried and a hand-prefixed query would double the prefix.
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
}

// e5NameRe matches the e5 embedding-model family by the model's own name
// segment: e5-large-v2, multilingual-e5-small, intfloat--multilingual-e5-small.
// The e5 retrieval contract is asymmetric — the models were trained with
// "query: " / "passage: " role prefixes — and dolmen embeds stored rows and
// search text through different calls, so the prefixes are added server-side
// rather than silently degrading ranking for every caller.
var e5NameRe = regexp.MustCompile(`(?i)(?:^|[-_])e5(?:[-_]|$)`)

// e5Prefixes reports the role prefixes the basic e5 contract requires for
// model, or empty strings when the model does not follow it. Only the
// model's own name segment decides (filepath.Base) — an org name or parent
// directory containing "e5" must not — and instruct-tuned variants
// (e5-mistral-7b-instruct, multilingual-e5-large-instruct) are excluded:
// they take task instructions, not these prefixes.
func e5Prefixes(model string) (query, passage string) {
	name := filepath.Base(model)
	if !e5NameRe.MatchString(name) || strings.Contains(strings.ToLower(name), "instruct") {
		return "", ""
	}
	return "query: ", "passage: "
}

// identityMarker returns "#e5" when dolmen applies the e5 prefix contract to
// model, "" otherwise. The marker versions the embedding identity: tables
// embedded before prefixes were applied carry the unmarked identity, so an
// upgraded server rejects them (and re-embeds via migrate) instead of
// silently mixing prefixed and raw vectors in one space.
func identityMarker(model string) string {
	if query, _ := e5Prefixes(model); query != "" {
		return "#e5"
	}
	return ""
}

// identityModel renders the model reference for an embedding identity.
// References without "%" or "#" are emitted verbatim — byte-identical to the
// identities dolmen has always produced, so existing tables keep matching.
// A reference containing either character is emitted in a versioned escaped
// form, "v2:" + percent-escaping, for two reasons. First, the escape makes
// the identity injective in the model (a directory literally named foo-e5#e5
// must not collide with an e5-detected foo-e5's "#e5" marker). Second, the
// "v2:" tag cannot occur in any legacy — unescaped — identity of a different
// model: Hub ids allow no ":" before their "/", and absolute paths start
// with "/", so an identity recorded by an earlier build can never equal a
// v2-tagged one (OpenAI-side model names are endpoint-defined and not
// validated; the tag is best-effort there).
func identityModel(model string) string {
	if !strings.ContainsAny(model, "%#") {
		return model
	}
	return "v2:" + strings.NewReplacer("%", "%25", "#", "%23").Replace(model)
}

// prefixAll returns texts with prefix prepended to each; the empty prefix
// returns texts unchanged.
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

// Identity pins tables to this endpoint and model. The base URL is stripped
// of HTTP userinfo (user:pass@): credentials authenticate requests to the
// endpoint, they do not name the embedding space, and the identity flows into
// describe_server responses and stored embed_space values, which must never
// expose secrets. A base URL that does not parse keeps its raw (trailing-slash
// trimmed) form — it cannot complete an HTTP request either, so it carries no
// working credentials to leak.
func (o *OpenAI) Identity() string {
	model := identityModel(o.Model) + identityMarker(o.Model)
	trimmed := strings.TrimRight(o.BaseURL, "/")
	u, err := url.Parse(trimmed)
	if err != nil {
		return o.Name() + "|" + trimmed + "|" + model
	}
	u.User = nil
	return o.Name() + "|" + u.String() + "|" + model
}

// Embed embeds stored-row text, prepending the e5 passage prefix for
// e5-family models — endpoints embed exactly the input they are sent.
func (o *OpenAI) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	_, passage := e5Prefixes(o.Model)
	return o.embed(ctx, texts, passage)
}

// EmbedQuery embeds one search text, prepending the e5 query prefix.
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
