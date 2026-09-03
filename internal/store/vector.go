package store

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/lsm/dolmen/internal/schema"
)

// VectorSearchResult is the outcome of a vector search. Skipped counts rows
// whose stored vector could not be scored — a corrupt blob, a dimension that
// disagrees with the query, or a non-finite component — so those rows are
// absent from Rows and a nonzero count means the search is partial.
type VectorSearchResult struct {
	Rows      []map[string]any
	Truncated bool
	Skipped   int
}

func (s *Store) SearchVector(ctx context.Context, nsName, table, column string, vec []float32, embedModel string, limit int, includeHidden bool) (VectorSearchResult, error) {
	n, err := s.ns(nsName)
	if err != nil {
		return VectorSearchResult{}, err
	}
	tx, err := n.ro.BeginTx(ctx, nil)
	if err != nil {
		return VectorSearchResult{}, err
	}
	defer tx.Rollback()
	sc, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return VectorSearchResult{}, err
	}
	column, dim, err := resolveVectorColumn(sc, table, column, embedModel)
	if err != nil {
		return VectorSearchResult{}, err
	}
	if dim > 0 && len(vec) != dim {
		return VectorSearchResult{}, invalidf("query vector has %d entries, column %s expects dim %d", len(vec), column, dim)
	}
	if !allFinite(vec) {
		return VectorSearchResult{}, invalidf("query vector contains a non-finite component")
	}
	limit = boundedLimit(limit)

	rows, err := tx.QueryContext(ctx,
		fmt.Sprintf(`SELECT id, %s FROM %s WHERE %s IS NOT NULL`, q(column), q(table), q(column)))
	if err != nil {
		return VectorSearchResult{}, err
	}
	defer rows.Close()

	type hit struct {
		id    int64
		score float64
	}
	var hits []hit
	skipped := 0
	for rows.Next() {
		var id int64
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return VectorSearchResult{}, err
		}
		stored, err := schema.DecodeVector(blob)
		if err != nil || len(stored) != len(vec) || !allFinite(stored) {
			skipped++
			continue
		}
		hits = append(hits, hit{id: id, score: cosine(vec, stored)})
	}
	if err := rows.Err(); err != nil {
		return VectorSearchResult{}, err
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score == hits[j].score {
			return hits[i].id < hits[j].id
		}
		return hits[i].score > hits[j].score
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	ids := make([]int64, len(hits))
	scoreByID := make(map[int64]float64, len(hits))
	for i, h := range hits {
		ids[i] = h.id
		scoreByID[h.id] = h.score
	}
	out, complete, err := fetchByIDs(ctx, tx, table, ids, projectionFromSchema(sc, includeHidden))
	if err != nil {
		return VectorSearchResult{}, err
	}
	for _, row := range out {
		if id, ok := row["id"].(int64); ok {
			row["_score"] = scoreByID[id]
		}
	}
	return VectorSearchResult{Rows: out, Truncated: !complete, Skipped: skipped}, nil
}

// resolveVectorColumn picks the column a vector search runs against and the
// dimension the query must have. embedModel is non-empty only for text
// queries, whose vector the active provider just embedded: those may only
// target the server-managed vectorize (_embedding) space, because cosine
// against a caller-provided vector column compares embeddings from an
// unrelated model and returns confident nonsense. Raw-vector queries may
// target any vector column — the caller owns matching the space.
func resolveVectorColumn(sc *schema.TableSchema, table, column, embedModel string) (string, int, error) {
	textQuery := embedModel != ""
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
		if textQuery && len(sc.VectorFields()) > 0 {
			return "", 0, invalidf("text queries search the table's vectorize (_embedding) space, but table %s has no vectorized field; its vector columns hold caller-provided embeddings, so pass a raw vector from the same embedding space to search them", table)
		}
		return "", 0, invalidf("table %s has no vector data (no vectorize field, no vector fields)", table)
	case column == "_embedding":
		if sc.VectorizeField() == nil {
			return "", 0, invalidf("table %s has no vectorize field for _embedding", table)
		}
		if sc.EmbedSpace != "" && embedModel != "" && sc.EmbedSpace != embedModel {
			return "", 0, invalidf("embedding model changed: table rows are embedded with %q but the provider now serves %q; re-embed via migrate (set_vectorize off, then on)", sc.EmbedSpace, embedModel)
		}
		dim = sc.EmbedDim
	case sc.Field(column) != nil && sc.Field(column).Type == schema.Vector:
		if textQuery {
			return "", 0, invalidf("text queries cannot target vector column %q of table %s: its embeddings are caller-provided and may come from an unrelated embedding space, so cosine against a freshly embedded query is meaningless; pass a raw vector from that space instead, or search the table's vectorize field", column, table)
		}
		dim = sc.Field(column).Dim
	default:
		return "", 0, invalidf("column %q is not a vector column", column)
	}
	return column, dim, nil
}

func (s *Store) ValidateVectorSearch(ctx context.Context, nsName, table, column, embedIdentity string) error {
	n, err := s.ns(nsName)
	if err != nil {
		return err
	}
	sc, err := loadSchema(ctx, n.ro, nsName, table)
	if err != nil {
		return err
	}
	_, _, err = resolveVectorColumn(sc, table, column, embedIdentity)
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
