package store

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/lsm/dolmen/internal/schema"
)

// VectorSearchResult is the outcome of a vector search. Skipped counts rows
// whose stored vector could not be scored — a corrupt blob, a dimension that
// disagrees with the query, a non-finite component, or a non-BLOB value an
// out-of-band writer left in the column — so those rows are absent from Rows
// and a nonzero count means the search is partial.
type VectorSearchResult struct {
	Rows      []map[string]any
	Truncated bool
	Skipped   int
}

// SearchVector executes a vector search (§6.2, §7). The engine is exact:
// Execution reports the brute-force path that served the query (Capabilities,
// still the 2b stub, will say the same once slice 4d makes it real). TODO(9d):
// scope and scopeIncarnation are ignored while auth is off — a non-nil scope
// will filter visible rows.
func (s *Store) SearchVector(ctx context.Context, nsName, table string, vq VectorQuery, includeHidden bool, scope *RowScope, scopeIncarnation Incarnation, page Page) (SearchResult, error) {
	n, err := s.ns(nsName)
	if err != nil {
		return SearchResult{}, err
	}
	tx, err := n.ro.BeginTx(ctx, nil)
	if err != nil {
		return SearchResult{}, err
	}
	defer tx.Rollback()
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

	query := fmt.Sprintf(`SELECT id, %s FROM %s WHERE %s IS NOT NULL`, q(column), q(table), q(column))
	var qargs []any
	if filter != "" {
		query = fmt.Sprintf(`%s AND (%s)`, query, filter)
		qargs = args
	}

	rows, err := tx.QueryContext(ctx, query, qargs...)
	if err != nil {
		return SearchResult{}, NewFilterError(filter, err)
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
	skipped := 0
	for rows.Next() {
		var id int64
		var raw any
		if err := rows.Scan(&id, &raw); err != nil {
			return SearchResult{}, err
		}
		// An out-of-band SQLite writer can corrupt a vector column with any
		// storage type (INTEGER, REAL, TEXT); only a BLOB of the right shape
		// is scoreable — everything else is skipped and counted, not fatal.
		blob, isBlob := raw.([]byte)
		if !isBlob {
			skipped++
			continue
		}
		stored, err := schema.DecodeVector(blob)
		if err != nil || len(stored) != len(vec) || !allFinite(stored) {
			skipped++
			continue
		}
		score := cosine(vec, stored)
		if score < threshold {
			continue
		}
		hits = append(hits, hit{id: id, score: score})
	}
	if err := rows.Err(); err != nil {
		return SearchResult{}, err
	}
	// Stable, deterministic ordering: higher score first, then lower id.
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score == hits[j].score {
			return hits[i].id < hits[j].id
		}
		return hits[i].score > hits[j].score
	})

	if offset > len(hits) {
		offset = len(hits)
	}
	end := offset + limit + 1
	if end > len(hits) {
		end = len(hits)
	}
	paged := hits[offset:end]

	// The (limit+1)th hit is only a look-ahead for truncated — never fetch
	// it, or an invalid value in that row would fail the whole page instead
	// of returning the valid rows with truncated=true.
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

// resolveVectorColumn picks the column a vector search runs against and the
// dimension the query must have. textQuery is true only for text queries,
// whose vector the active provider embeds: those may only target the
// server-managed vectorize (_embedding) space, because cosine against a
// caller-provided vector column compares embeddings from an unrelated model
// and returns confident nonsense. Raw-vector queries may target any vector
// column — the caller owns matching the space. textQuery is independent of
// embedModel so a table's shape can be validated even when no provider is
// configured (embedModel "") — the two failures must report distinct errors.
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

// vectorColumnNames lists the table's declared vector columns, comma-joined,
// or "" when there are none — the raw-vector fallback in an error message is
// only offered when this is non-empty.
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

// ValidateVectorQuery validates a TEXT vector query against a TableState
// snapshot (§6.2): vectorize field present and embed-space identity pinned —
// the pre-embed check the API layer runs against the same snapshot it will
// resolve scopes from, so an invalid query fails before the embedding
// provider is contacted (or billed) (§7). It is deliberately a function over
// the snapshot, not an Engine method: the validation travels with the schema
// the caller already holds. embedModel is the active provider's identity (""
// when none is configured — the table's shape still validates).
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
