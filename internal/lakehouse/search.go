package lakehouse

import (
	"context"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type rankedHit struct {
	id    int64
	score float64
}

func searchLimit(page store.Page) (int, error) {
	if page.Offset < 0 {
		return 0, invalidf("offset must be non-negative")
	}
	limit := store.DefaultSearchLimit
	if page.Limit > 0 {
		limit = page.Limit
	}
	if limit > store.MaxSearchLimit {
		limit = store.MaxSearchLimit
	}
	return limit, nil
}

func (s *Store) candidates(ctx context.Context, n *namespace, ns string, state tableState, filter string, args []any, scope *store.RowScope) (map[int64]bool, error) {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return nil, nil
	}
	if strings.Contains(filter, ";") {
		return nil, invalidf("multiple statements are not allowed in filter")
	}
	ids, err := s.matchIDs(ctx, n, ns, state, filter, args, scope)
	if err != nil {
		return nil, err
	}
	return idSet(ids), nil
}

func (s *Store) presentRanked(ctx context.Context, n *namespace, state tableState, rows map[int64]map[string]any, hits []rankedHit, page store.Page, limit int, includeHidden bool) ([]map[string]any, bool, error) {
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score == hits[j].score {
			return hits[i].id < hits[j].id
		}
		return hits[i].score > hits[j].score
	})
	offset := min(page.Offset, len(hits))
	end := min(offset+limit+1, len(hits))
	paged := hits[offset:end]
	more := len(paged) > limit
	if more {
		paged = paged[:limit]
	}
	reveal, err := store.RevealSet(ctx, state.schema, s.secrets)
	if err != nil {
		return nil, false, err
	}
	raws := make([]map[string]any, len(paged))
	for i, h := range paged {
		raws[i] = rows[h.id]
	}
	sealed, err := loadSecrets(ctx, n, state, reveal, raws)
	if err != nil {
		return nil, false, err
	}
	out := []map[string]any{}
	total := 0
	for i, h := range paged {
		row, size, err := s.presentRow(state.schema, reveal, sealed[h.id], raws[i], includeHidden)
		if err != nil {
			return nil, false, err
		}
		if total+size > store.MaxQueryBytes {
			if len(out) == 0 {
				return nil, false, invalidf("search result exceeds the %d MiB response budget on its first row", store.MaxQueryBytes>>20)
			}
			return out, true, nil
		}
		total += size
		row["_score"] = h.score
		out = append(out, row)
	}
	return out, more, nil
}

func (s *Store) searchState(ctx context.Context, n *namespace, ns, name string, scope *store.RowScope, expected store.Incarnation) (tableState, error) {
	state, err := loadTable(ctx, n, ns, name)
	if err != nil {
		return state, err
	}
	if err := checkExpected(state, expected, false); err != nil {
		return state, err
	}
	if scope != nil && !scope.Empty && !state.schema.HasOwner {
		return state, invalidf("table %s carries no owner column, so a row scope cannot be applied to it", name)
	}
	return state, nil
}

func (s *Store) SearchFulltext(ctx context.Context, ns, name, match, filter string, args []any, includeHidden bool, scope *store.RowScope, scopeIncarnation store.Incarnation, page store.Page) (store.SearchResult, error) {
	if err := store.ValidateFulltextQuery(match); err != nil {
		return store.SearchResult{}, err
	}
	if len(args) > 100 {
		return store.SearchResult{}, invalidf("too many filter arguments")
	}
	limit, err := searchLimit(page)
	if err != nil {
		return store.SearchResult{}, err
	}
	q, err := compileQuery(match)
	if err != nil {
		return store.SearchResult{}, err
	}
	result := store.SearchResult{Rows: []map[string]any{}}
	err = s.withNamespace(ctx, ns, func(n *namespace) error {
		state, err := s.searchState(ctx, n, ns, name, scope, scopeIncarnation)
		if err != nil {
			return err
		}
		fts := state.schema.FTSFields()
		if len(fts) == 0 {
			return invalidf("table %s has no fulltext fields; mark one with migrate (set_fulltext) before searching it", name)
		}
		allowed, err := s.candidates(ctx, n, ns, state, filter, args, scope)
		if err != nil {
			return err
		}
		raws, err := scanRows(ctx, state, nil)
		if err != nil {
			return err
		}
		rows := make(map[int64]map[string]any, len(raws))
		docs := make([]*ftsDoc, 0, len(raws))
		for _, raw := range raws {
			id := raw["id"].(int64)
			rows[id] = raw
			doc := &ftsDoc{id: id}
			for _, f := range fts {
				text, _ := raw[f.Name].(string)
				tokens := analyze(text)
				doc.fields = append(doc.fields, tokens)
				doc.length += len(tokens)
			}
			docs = append(docs, doc)
		}
		stats := corpusStats(q, docs)
		var hits []rankedHit
		for _, doc := range docs {
			if allowed != nil && !allowed[doc.id] || !inScope(state.schema, scope, rows[doc.id]) || !q.matches(doc) {
				continue
			}
			hits = append(hits, rankedHit{id: doc.id, score: bm25(q, stats, doc)})
		}
		result.Rows, result.Truncated, err = s.presentRanked(ctx, n, state, rows, hits, page, limit, includeHidden)
		return err
	})
	if err != nil {
		return store.SearchResult{}, err
	}
	return result, nil
}

func (s *Store) SearchVector(ctx context.Context, ns, name string, q store.VectorQuery, includeHidden bool, scope *store.RowScope, scopeIncarnation store.Incarnation, page store.Page) (store.SearchResult, error) {
	if len(q.Args) > 100 {
		return store.SearchResult{}, invalidf("too many filter arguments")
	}
	limit, err := searchLimit(page)
	if err != nil {
		return store.SearchResult{}, err
	}
	threshold := math.Inf(-1)
	if q.MinScore != nil {
		threshold = *q.MinScore
	}
	result := store.SearchResult{Rows: []map[string]any{}, Execution: store.VectorExact}
	err = s.withNamespace(ctx, ns, func(n *namespace) error {
		state, err := s.searchState(ctx, n, ns, name, scope, scopeIncarnation)
		if err != nil {
			return err
		}
		column, dim, err := store.ResolveVectorColumn(state.schema, name, q.Column, q.EmbedModel != "", q.EmbedModel)
		if err != nil {
			return err
		}
		if dim > 0 && len(q.Vec) != dim {
			return invalidf("query vector has %d entries, column %s expects dim %d", len(q.Vec), column, dim)
		}
		if !store.AllFinite(q.Vec) {
			return invalidf("query vector contains a non-finite component")
		}
		allowed, err := s.candidates(ctx, n, ns, state, q.Filter, q.Args, scope)
		if err != nil {
			return err
		}
		raws, err := scanRows(ctx, state, nil)
		if err != nil {
			return err
		}
		rows := make(map[int64]map[string]any, len(raws))
		var hits []rankedHit
		for _, raw := range raws {
			id := raw["id"].(int64)
			if allowed != nil && !allowed[id] || !inScope(state.schema, scope, raw) {
				continue
			}
			blob, ok := raw[column].([]byte)
			if !ok {
				continue
			}
			stored, err := schema.DecodeVector(blob)
			if err != nil || len(stored) != len(q.Vec) || !store.AllFinite(stored) {
				result.SkippedVectors++
				continue
			}
			score := store.Cosine(q.Vec, stored)
			if score < threshold {
				continue
			}
			rows[id] = raw
			hits = append(hits, rankedHit{id: id, score: score})
		}
		result.Rows, result.Truncated, err = s.presentRanked(ctx, n, state, rows, hits, page, limit, includeHidden)
		return err
	})
	if err != nil {
		return store.SearchResult{}, err
	}
	return result, nil
}

func (s *Store) Tokenize(ctx context.Context, ns, name, text string, expected store.Incarnation) ([]string, error) {
	if n := utf8.RuneCountInString(text); n > store.MaxTokenizeRunes {
		return nil, invalidf("text is %d characters; tokenize takes at most %d, which covers any query or field value worth checking", n, store.MaxTokenizeRunes)
	}
	var out []string
	err := s.withNamespace(ctx, ns, func(n *namespace) error {
		state, err := loadTable(ctx, n, ns, name)
		if err != nil {
			return err
		}
		if err := checkExpected(state, expected, false); err != nil {
			return err
		}
		if len(state.schema.FTSFields()) == 0 {
			return invalidf("table %s has no fulltext fields, so it has no index to tokenize for; mark a string or text field fulltext with migrate (set_fulltext)", name)
		}
		out = Tokens(text)
		return nil
	})
	return out, err
}
