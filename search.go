package dolmen

import (
	"context"
	"errors"
	"math"
	"strings"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/ops"
	"github.com/lsm/dolmen/internal/store"
)

type SearchOptions struct {
	Filter        string
	Args          []any
	Offset        int
	Limit         int
	IncludeHidden bool
	Reveal        []string
}

type VectorQuery struct {
	Text     string
	Vec      []float32
	Column   string
	MinScore *float64
}

type SearchResult struct {
	Rows           []map[string]any
	Truncated      bool
	SkippedVectors int
}

const noProviderHelp = "open the store with a usable embedding provider (WithEmbedding)"

func validateSearchOptions(opts SearchOptions) error {
	if opts.Limit != 0 && (opts.Limit < 1 || opts.Limit > store.MaxSearchLimit) {
		return derr.New(derr.InvalidRequest, "SearchOptions.Limit must be between 1 and %d (0 keeps the default page size), got %d", store.MaxSearchLimit, opts.Limit)
	}
	if opts.Offset < 0 || opts.Offset > maxOffsetPerCall {
		return derr.New(derr.InvalidRequest, "SearchOptions.Offset must be between 0 and %d, got %d", maxOffsetPerCall, opts.Offset)
	}
	if len(opts.Args) > maxArgsPerCall {
		return derr.New(derr.InvalidRequest, "at most %d filter bind parameters are accepted per call, got %d", maxArgsPerCall, len(opts.Args))
	}
	if opts.Filter != "" && (strings.TrimSpace(opts.Filter) == "" || strings.Contains(opts.Filter, ";")) {
		return derr.New(derr.InvalidRequest, "filter must be a non-blank SQL WHERE expression without semicolons")
	}
	return nil
}

func (s *Store) SearchFulltext(ctx context.Context, namespace, table, query string, opts SearchOptions) (r0 SearchResult, err error) {
	ctx, span := s.startOp(ctx, "search_fulltext", namespace, table)
	defer func() { endOp(span, err) }()
	if err := s.begin(); err != nil {
		return SearchResult{}, err
	}
	defer s.done()
	if err := ctx.Err(); err != nil {
		return SearchResult{}, facadeErr(err)
	}
	if err := validateSearchOptions(opts); err != nil {
		return SearchResult{}, err
	}
	if strings.TrimSpace(query) == "" {
		return SearchResult{}, derr.New(derr.InvalidRequest, "query must contain a non-whitespace FTS5 MATCH expression")
	}
	if tbl := ops.NormalizeTable(table); tbl == "" || !validTableName(tbl) {
		return SearchResult{}, derr.New(derr.InvalidRequest, "table must match ^[a-z][a-z0-9_]{0,63}$ and not contain __fts or start with sqlite_")
	}
	ns := ops.NormalizeNamespace(namespace)
	args := append([]any(nil), opts.Args...)
	ctx = store.WithReveal(ctx, opts.Reveal)
	res, err := s.eng.SearchFulltext(ctx, ns, ops.NormalizeTable(table), query, opts.Filter, args,
		opts.IncludeHidden, nil, store.Incarnation{}, store.Page{Offset: opts.Offset, Limit: opts.Limit})
	if err != nil {
		return SearchResult{}, facadeErr(err)
	}
	return SearchResult{Rows: res.Rows, Truncated: res.Truncated, SkippedVectors: res.SkippedVectors}, nil
}

func (s *Store) SearchVector(ctx context.Context, namespace, table string, query VectorQuery, opts SearchOptions) (r0 SearchResult, err error) {
	ctx, span := s.startOp(ctx, "search_vector", namespace, table)
	defer func() { endOp(span, err) }()
	if err := s.begin(); err != nil {
		return SearchResult{}, err
	}
	defer s.done()
	if err := ctx.Err(); err != nil {
		return SearchResult{}, facadeErr(err)
	}
	if err := validateSearchOptions(opts); err != nil {
		return SearchResult{}, err
	}
	if query.MinScore != nil && (math.IsNaN(*query.MinScore) || math.IsInf(*query.MinScore, 0)) {
		return SearchResult{}, derr.New(derr.InvalidRequest, "MinScore must be a finite number (NaN and infinities silently filter everything or nothing)")
	}
	if query.Text != "" && query.Vec != nil {
		return SearchResult{}, derr.New(derr.InvalidRequest, "pass either text or vector, not both")
	}
	if query.Vec != nil && len(query.Vec) == 0 {
		return SearchResult{}, derr.New(derr.InvalidRequest, "vector must have at least one element")
	}
	if tbl := ops.NormalizeTable(table); tbl == "" || !validTableName(tbl) {
		return SearchResult{}, derr.New(derr.InvalidRequest, "table must match ^[a-z][a-z0-9_]{0,63}$ and not contain __fts or start with sqlite_")
	}
	var vec []float64
	if query.Vec != nil {
		vec = make([]float64, len(query.Vec))
		for i, x := range query.Vec {
			vec[i] = float64(x)
		}
	}
	emb := ops.EmbeddingProvider(disabledEmbedding{})
	if s.emb != nil {
		emb = s.tracing.Embedder(s.emb)
	}
	args := append([]any(nil), opts.Args...)
	vq, err := ops.PrepareVectorQuery(ctx, s.eng, ops.NormalizeNamespace(namespace), ops.NormalizeTable(table), ops.VectorQuery{
		Column:   query.Column,
		Text:     query.Text,
		Vec:      vec,
		Filter:   opts.Filter,
		Args:     args,
		MinScore: query.MinScore,
	}, emb, noProviderHelp)
	if err != nil {
		return SearchResult{}, facadeErr(err)
	}
	ctx = store.WithReveal(ctx, opts.Reveal)
	res, err := s.eng.SearchVector(ctx, ops.NormalizeNamespace(namespace), ops.NormalizeTable(table), vq,
		opts.IncludeHidden, nil, store.Incarnation{}, store.Page{Offset: opts.Offset, Limit: opts.Limit})
	if err != nil {
		return SearchResult{}, facadeErr(err)
	}
	return SearchResult{Rows: res.Rows, Truncated: res.Truncated, SkippedVectors: res.SkippedVectors}, nil
}

type disabledEmbedding struct{}

var errEmbeddingDisabled = errors.New("dolmen: embedding is disabled (no provider was supplied to WithEmbedding)")

func (disabledEmbedding) Identity() string { return "" }

func (disabledEmbedding) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return nil, errEmbeddingDisabled
}

func (disabledEmbedding) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	return nil, errEmbeddingDisabled
}
