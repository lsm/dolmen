package store

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/lsm/dolmen/internal/schema"
)

func (s *Store) SearchVector(ctx context.Context, nsName, table, column string, vec []float32, embedModel string, limit int, includeHidden bool, filter string, args []any, minScore *float64) ([]map[string]any, bool, error) {
	n, err := s.ns(nsName)
	if err != nil {
		return nil, false, err
	}
	tx, err := n.ro.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	sc, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return nil, false, err
	}
	column, dim, err := resolveVectorColumn(sc, table, column, embedModel)
	if err != nil {
		return nil, false, err
	}
	if dim > 0 && len(vec) != dim {
		return nil, false, invalidf("query vector has %d entries, column %s expects dim %d", len(vec), column, dim)
	}
	for _, x := range vec {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return nil, false, invalidf("query vector contains a non-finite component")
		}
	}
	filter = strings.TrimSpace(filter)
	if filter != "" {
		if strings.Contains(filter, ";") {
			return nil, false, invalidf("multiple statements are not allowed in filter")
		}
		if len(args) > 100 {
			return nil, false, invalidf("too many filter arguments")
		}
		for i, a := range args {
			args[i] = normalizeArg(a)
		}
	}
	limit = boundedLimit(limit)

	query := fmt.Sprintf(`SELECT id, %s FROM %s WHERE %s IS NOT NULL`, q(column), q(table), q(column))
	var qargs []any
	if filter != "" {
		query = fmt.Sprintf(`%s AND (%s)`, query, filter)
		qargs = args
	}

	rows, err := tx.QueryContext(ctx, query, qargs...)
	if err != nil {
		return nil, false, NewQueryError(filter, err)
	}
	defer rows.Close()

	threshold := math.Inf(-1)
	if minScore != nil {
		threshold = *minScore
	}

	type hit struct {
		id    int64
		score float64
	}
	var hits []hit
	for rows.Next() {
		var id int64
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, false, err
		}
		stored, err := schema.DecodeVector(blob)
		if err != nil || len(stored) != len(vec) {
			continue
		}
		score := cosine(vec, stored)
		if score < threshold {
			continue
		}
		hits = append(hits, hit{id: id, score: score})
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
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
		return nil, false, err
	}
	for _, row := range out {
		if id, ok := row["id"].(int64); ok {
			row["_score"] = scoreByID[id]
		}
	}
	return out, !complete, nil
}

func resolveVectorColumn(sc *schema.TableSchema, table, column, embedModel string) (string, int, error) {
	if column == "" {
		if vf := sc.VectorizeField(); vf != nil {
			column = "_embedding"
		} else if vfs := sc.VectorFields(); len(vfs) > 0 {
			column = vfs[0].Name
		} else {
			return "", 0, invalidf("table %s has no vector data (no vectorize field, no vector fields)", table)
		}
	}
	var dim int
	switch {
	case column == "_embedding":
		if sc.VectorizeField() == nil {
			return "", 0, invalidf("table %s has no vectorize field for _embedding", table)
		}
	case sc.Field(column) != nil && sc.Field(column).Type == schema.Vector:
		dim = sc.Field(column).Dim
	default:
		return "", 0, invalidf("column %q is not a vector column", column)
	}
	if column == "_embedding" {
		if sc.EmbedSpace != "" && embedModel != "" && sc.EmbedSpace != embedModel {
			return "", 0, invalidf("embedding model changed: table rows are embedded with %q but the provider now serves %q; re-embed via migrate (set_vectorize off, then on)", sc.EmbedSpace, embedModel)
		}
		dim = sc.EmbedDim
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
