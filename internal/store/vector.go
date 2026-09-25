package store

import (
	"container/heap"
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/lsm/dolmen/internal/schema"
)

type VectorSearchResult struct {
	Rows      []map[string]any
	Truncated bool
	Skipped   int
}

func (s *Store) SearchVector(ctx context.Context, nsName, table string, vq VectorQuery, includeHidden bool, scope *RowScope, scopeIncarnation Incarnation, page Page) (SearchResult, error) {
	n, err := s.ns(nsName)
	if err != nil {
		return SearchResult{}, err
	}
	defer n.unpin()
	tx, done, err := beginCallerTx(ctx, n.ro, nil)
	if err != nil {
		return SearchResult{}, err
	}
	defer done()
	sc, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return SearchResult{}, err
	}
	column, dim, err := resolveVectorColumn(sc, table, vq.Column, vq.EmbedModel != "", vq.EmbedModel)
	if err != nil {
		return SearchResult{}, err
	}
	if dim > 0 && len(vq.Vec) != dim {
		return SearchResult{}, invalidf("query vector has %d entries, column %s expects dim %d", len(vq.Vec), column, dim)
	}
	if !allFinite(vq.Vec) {
		return SearchResult{}, invalidf("query vector contains a non-finite component")
	}
	limit := searchLimit(page.Limit)
	offset := page.Offset
	if offset < 0 {
		return SearchResult{}, invalidf("offset must be non-negative")
	}
	filter := strings.TrimSpace(vq.Filter)
	args := vq.Args
	if filter != "" {
		if strings.Contains(filter, ";") {
			return SearchResult{}, invalidf("multiple statements are not allowed in filter")
		}
		if len(args) > 100 {
			return SearchResult{}, invalidf("too many filter arguments")
		}
		for i, a := range args {
			args[i] = normalizeArg(a)
		}
	}
	vec := vq.Vec
	minScore := vq.MinScore

	if err := checkScopeIncarnation(ctx, tx, nsName, table, scopeIncarnation); err != nil {
		return SearchResult{}, err
	}
	if err := scopeUsable(scope, sc); err != nil {
		return SearchResult{}, err
	}
	threshold := math.Inf(-1)
	if minScore != nil {
		threshold = *minScore
	}
	var hits []vecHit
	skipped := 0
	cached := false
	if s.vcache.max > 0 {
		var candidates func() ([]int64, error)
		if filter != "" {
			candidates = func() ([]int64, error) {
				return candidateIDs(ctx, tx, table, column, filter, args, scope)
			}
		}
		hits, skipped, cached, err = s.vcache.score(ctx, tx, nsName, table, column, vec, threshold, candidates, scope, sc.HasOwner)
		if err != nil {
			return SearchResult{}, err
		}
	}
	if !cached {
		if hits, skipped, err = scanVectors(ctx, tx, table, column, filter, args, scope, vec, threshold); err != nil {
			return SearchResult{}, err
		}
	}

	k := offset + limit + 1
	if offset >= len(hits) || k > len(hits) {
		k = len(hits)
	}
	hits = topHits(hits, k)

	if offset > len(hits) {
		offset = len(hits)
	}
	end := offset + limit + 1
	if end > len(hits) {
		end = len(hits)
	}
	paged := hits[offset:end]

	hasMore := len(paged) > limit
	if hasMore {
		paged = paged[:limit]
	}

	ids := make([]int64, len(paged))
	scoreByID := make(map[int64]float64, len(paged))
	for i, h := range paged {
		ids[i] = h.id
		scoreByID[h.id] = h.score
	}
	out, complete, err := fetchByIDs(ctx, tx, table, ids, projectionFromSchema(sc, includeHidden))
	if err != nil {
		return SearchResult{}, err
	}
	for _, row := range out {
		if id, ok := row["id"].(int64); ok {
			row["_score"] = scoreByID[id]
		}
	}
	return SearchResult{
		Rows:           out,
		Truncated:      hasMore || !complete,
		SkippedVectors: skipped,
		Execution:      VectorExact,
	}, nil
}

func resolveVectorColumn(sc *schema.TableSchema, table, column string, textQuery bool, embedModel string) (string, int, error) {
	if column == "" && sc.VectorizeField() != nil {
		column = "_embedding"
	} else if column == "" && !textQuery {
		if vfs := sc.VectorFields(); len(vfs) > 0 {
			column = vfs[0].Name
		}
	}
	var dim int
	switch {
	case column == "":
		if textQuery {
			if names := vectorColumnNames(sc); names != "" {
				return "", 0, invalidf("text queries search the server-managed vectorize (_embedding) space, but table %s has no vectorized field; add one via migrate (set_vectorize on a string or text field), or search a declared vector column (%s) with a raw vector from the same embedding space that produced the stored vectors", table, names)
			}
			return "", 0, invalidf("text queries need a vectorize field, but table %s has none and it has no vector columns to search with raw vectors; add one via migrate (set_vectorize on a string or text field)", table)
		}
		return "", 0, invalidf("table %s has no vector data (no vectorize field, no vector fields)", table)
	case column == "_embedding":
		if sc.VectorizeField() == nil {
			if names := vectorColumnNames(sc); names != "" {
				return "", 0, invalidf("table %s has no vectorize field, so there is no _embedding space to search; add one via migrate (set_vectorize on a string or text field), or search a declared vector column (%s) with a raw vector query", table, names)
			}
			return "", 0, invalidf("table %s has no vectorize field, so there is no _embedding space to search; add one via migrate (set_vectorize on a string or text field)", table)
		}
		if sc.EmbedSpace != "" && embedModel != "" && sc.EmbedSpace != embedModel {
			return "", 0, invalidf("embedding model changed: table rows are embedded with %q but the provider now serves %q; re-embed via migrate (set_vectorize off, then on)", sc.EmbedSpace, embedModel)
		}
		dim = sc.EmbedDim
	case sc.Field(column) != nil && sc.Field(column).Type == schema.Vector:
		if textQuery {
			if sc.VectorizeField() != nil {
				return "", 0, invalidf("text queries cannot target vector column %q of table %s: its embeddings are caller-provided and may come from an unrelated embedding space, so cosine against a freshly embedded query is meaningless; pass a raw vector from that space instead, or search the table's vectorize field", column, table)
			}
			return "", 0, invalidf("text queries cannot target vector column %q of table %s: its embeddings are caller-provided and may come from an unrelated embedding space, so cosine against a freshly embedded query is meaningless; pass a raw vector from that space instead — or add a vectorize field via migrate (set_vectorize on a string or text field) to make the table searchable by text", column, table)
		}
		dim = sc.Field(column).Dim
	default:
		return "", 0, invalidf("column %q is not a vector column", column)
	}
	return column, dim, nil
}

func vectorColumnNames(sc *schema.TableSchema) string {
	cols := sc.VectorFields()
	if len(cols) == 0 {
		return ""
	}
	names := make([]string, len(cols))
	for i, f := range cols {
		names[i] = f.Name
	}
	return strings.Join(names, ", ")
}

func ValidateVectorQuery(sc *schema.TableSchema, table, column, embedModel string) error {
	_, _, err := resolveVectorColumn(sc, table, column, true, embedModel)
	return err
}

func allFinite(v []float32) bool {
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return false
		}
	}
	return true
}

func cosineKnown(a, b []float32, na, nb float64) float64 {
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func ResolveVectorColumn(sc *schema.TableSchema, table, column string, textQuery bool, embedModel string) (string, int, error) {
	return resolveVectorColumn(sc, table, column, textQuery, embedModel)
}

func Cosine(a, b []float32) float64 { return cosine(a, b) }

func AllFinite(v []float32) bool { return allFinite(v) }

type vecHit struct {
	id    int64
	score float64
}

func decodeStoredVector(raw any) ([]float32, bool) {
	blob, isBlob := raw.([]byte)
	if !isBlob {
		return nil, false
	}
	stored, err := schema.DecodeVector(blob)
	if err != nil || !allFinite(stored) {
		return nil, false
	}
	return stored, true
}

func vectorSource(table, column, filter string, args []any, scope *RowScope, cols string) (string, []any) {
	if filter == "" {
		clause, scopeArgs := scopeClause(scope, "")
		if clause != "" {
			return fmt.Sprintf(`SELECT %s FROM %s WHERE %s AND %s IS NOT NULL`, cols, q(table), clause, q(column)), scopeArgs
		}
		return fmt.Sprintf(`SELECT %s FROM %s WHERE %s IS NOT NULL`, cols, q(table), q(column)), scopeArgs
	}
	prefix, source, scopeArgs := scopedSource(table, scope)
	query := fmt.Sprintf(`%sSELECT %s FROM %s WHERE %s IS NOT NULL AND (%s)`, prefix, cols, source, q(column), filter)
	return query, append(append(make([]any, 0, len(scopeArgs)+len(args)), scopeArgs...), args...)
}

func candidateIDs(ctx context.Context, tx rowsQuerier, table, column, filter string, args []any, scope *RowScope) ([]int64, error) {
	query, qargs := vectorSource(table, column, filter, args, scope, "id")
	rows, err := tx.QueryContext(ctx, query, qargs...)
	if err != nil {
		return nil, NewFilterError(filter, err)
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, NewFilterError(filter, err)
	}
	return ids, nil
}

func scanVectors(ctx context.Context, tx rowsQuerier, table, column, filter string, args []any, scope *RowScope, vec []float32, threshold float64) ([]vecHit, int, error) {
	query, qargs := vectorSource(table, column, filter, args, scope, "id, "+q(column))
	rows, err := tx.QueryContext(ctx, query, qargs...)
	if err != nil {
		return nil, 0, NewFilterError(filter, err)
	}
	defer rows.Close()
	var hits []vecHit
	skipped := 0
	for rows.Next() {
		var id int64
		var raw any
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, 0, err
		}
		stored, ok := decodeStoredVector(raw)
		if !ok || len(stored) != len(vec) {
			skipped++
			continue
		}
		if score := cosine(vec, stored); score >= threshold {
			hits = append(hits, vecHit{id: id, score: score})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, NewFilterError(filter, err)
	}
	return hits, skipped, nil
}

func ranksBefore(a, b vecHit) bool {
	if a.score == b.score {
		return a.id < b.id
	}
	return a.score > b.score
}

type worstFirst []vecHit

func (h worstFirst) Len() int           { return len(h) }
func (h worstFirst) Less(i, j int) bool { return ranksBefore(h[j], h[i]) }
func (h worstFirst) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *worstFirst) Push(x any)        { *h = append(*h, x.(vecHit)) }
func (h *worstFirst) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

func topHits(hits []vecHit, k int) []vecHit {
	if k <= 0 {
		return hits[:0]
	}
	if k < len(hits)/4 {
		h := make(worstFirst, 0, k)
		for _, x := range hits {
			if len(h) < k {
				heap.Push(&h, x)
			} else if ranksBefore(x, h[0]) {
				h[0] = x
				heap.Fix(&h, 0)
			}
		}
		hits = h
	}
	sort.Slice(hits, func(i, j int) bool { return ranksBefore(hits[i], hits[j]) })
	return hits
}
