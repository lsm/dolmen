package postgres

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
	"github.com/lsm/dolmen/internal/value"
)

type rowProjection struct {
	fields     []schema.Field
	columns    []string
	labelBytes int
	reveal     map[string]bool
	store      *Store
}

func projectRows(state tableState, includeHidden bool) rowProjection {
	fields := append([]schema.Field{{Name: "id", Type: schema.Number}, {Name: "created_at", Type: schema.Timestamp}}, state.schema.Fields...)
	physical := make([]string, len(fields))
	for i := range fields {
		if i < 2 {
			physical[i] = fields[i].Name
			continue
		}
		physical[i] = state.columns[fields[i].Name]
	}
	if includeHidden && state.schema.VectorizeField() != nil {
		fields = append(fields, schema.Field{Name: "_embedding", Type: schema.Vector, Dim: state.schema.EmbedDim})
		physical = append(physical, "_embedding")
	}
	if state.schema.HasOwner {
		fields = append(fields, schema.Field{Name: schema.OwnerColumn, Type: schema.Text})
		physical = append(physical, schema.OwnerColumn)
	}
	p := rowProjection{fields: fields, columns: make([]string, len(fields))}
	for i, f := range fields {
		p.columns[i] = ident(physical[i])
		if f.Type == schema.Number && i >= 2 {
			p.columns[i] += "::text"
		}
		p.labelBytes += value.EncodedSize(f.Name) + 16
	}
	return p
}

func (p rowProjection) scan(rows pgx.Rows, budgetLabel string) ([]map[string]any, bool, error) {
	out := []map[string]any{}
	total := 0
	for rows.Next() {
		raw, err := rows.Values()
		if err != nil {
			return nil, false, err
		}
		row := make(map[string]any, len(p.fields))
		size := 0
		for i, f := range p.fields {
			v := raw[i]
			if f.Type == schema.Number && v != nil && i >= 2 {
				v, err = decodeNumber(f, v.(string))
				if err != nil {
					return nil, false, fmt.Errorf("%w: column %q contains an invalid number", store.ErrInvalid, f.Name)
				}
			}
			if b, ok := v.([]byte); ok && len(b) > store.MaxQueryBytes {
				return nil, false, fmt.Errorf("%w: column %q exceeds the %d MiB response budget; select fewer or smaller columns", store.ErrInvalid, f.Name, store.MaxQueryBytes>>20)
			}
			if total+size+value.RawSize(v) > store.MaxQueryBytes {
				if len(out) == 0 {
					return nil, false, fmt.Errorf("%w: %s exceeds the %d MiB response budget on its first row", store.ErrInvalid, budgetLabel, store.MaxQueryBytes>>20)
				}
				return out, true, nil
			}
			decoded := value.Decode(f.Type, v)
			if plain, ok, err := p.store.presentSecret(p.reveal, f, v); err != nil {
				return nil, false, err
			} else if ok {
				decoded = plain
			}
			presented := value.ApproxSize(decoded)
			if f.Type == schema.Vector {
				if vec, ok := decoded.([]float64); ok {
					presented = len(vec)*27 + 8
				}
			} else if f.Type == schema.JSON {
				if _, ok := decoded.(string); !ok {
					presented = value.RawSize(v)
				}
			}
			size += presented
			if total+size+p.labelBytes > store.MaxQueryBytes {
				if len(out) == 0 {
					return nil, false, fmt.Errorf("%w: %s exceeds the %d MiB response budget on its first row", store.ErrInvalid, budgetLabel, store.MaxQueryBytes>>20)
				}
				return out, true, nil
			}
			row[f.Name] = decoded
		}
		total += size + p.labelBytes
		out = append(out, row)
	}
	return out, false, rows.Err()
}

func (s *Store) fetchRanked(ctx context.Context, tx pgx.Tx, n namespace, state tableState, ids []int64, includeHidden bool) ([]map[string]any, bool, error) {
	if len(ids) == 0 {
		return []map[string]any{}, false, nil
	}
	p := projectRows(state, includeHidden)
	reveal, err := s.revealSet(ctx, state.schema)
	if err != nil {
		return nil, false, err
	}
	p.reveal, p.store = reveal, s
	stmt := "SELECT " + strings.Join(p.columns, ",") + " FROM " + ident(n.physical, state.physical) +
		" JOIN unnest($1::bigint[]) WITH ORDINALITY AS ranked(rid, pos) ON ranked.rid = id ORDER BY ranked.pos"
	rows, err := tx.Query(ctx, stmt, ids)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	return p.scan(rows, "search result")
}

func (s *Store) SearchFulltext(ctx context.Context, ns, table, match, filter string, args []any, includeHidden bool, scope *store.RowScope, scopeIncarnation store.Incarnation, page store.Page) (store.SearchResult, error) {
	if page.Offset < 0 {
		return store.SearchResult{}, invalidf("offset must be non-negative")
	}
	if len(args) > 100 {
		return store.SearchResult{}, invalidf("too many filter arguments")
	}
	limit := store.DefaultSearchLimit
	if page.Limit > 0 {
		limit = page.Limit
	}
	if limit > store.MaxSearchLimit {
		limit = store.MaxSearchLimit
	}
	filter = strings.TrimSpace(filter)
	args, err := queryArgs(args)
	if err != nil {
		return store.SearchResult{}, err
	}
	result := store.SearchResult{Rows: []map[string]any{}}
	err = s.read(ctx, ns, func(tx pgx.Tx, n namespace) error {
		state, err := s.loadTable(ctx, tx, n, table)
		if err != nil {
			return err
		}
		if err := s.guardScope(ctx, tx, n, table, state, scopeIncarnation); err != nil {
			return err
		}
		if err := scopeUsable(scope, state.schema); err != nil {
			return err
		}
		if len(fulltextFields(state.schema.Fields)) == 0 {
			return invalidf("table %s has no fulltext fields", table)
		}
		bound := []any{}
		compiled := ""
		if filter != "" {
			var err error
			compiled, bound, err = s.compileMutationFilter(ctx, tx, n, filter, args, state, scope)
			if err != nil {
				return err
			}
		}
		tsquery, tsargs, err := compileFTSQuery(match, len(bound))
		if err != nil {
			return err
		}
		physical := ident(n.physical, state.physical)
		bind := append(append([]any{}, bound...), tsargs...)
		where := ident(ftsColumn) + " @@ " + tsquery
		if compiled != "" {
			where += " AND id IN (" + compiled + ")"
		}
		if clause, sargs := scopePredicate(scope, "", len(bind)+1); clause != "" {
			where += " AND " + clause
			bind = append(bind, sargs...)
		}
		rank := "ts_rank_cd(" + ident(ftsColumn) + "," + tsquery + ")"
		bind = append(bind, limit+1, page.Offset)
		stmt := "SELECT id FROM " + physical + " WHERE " + where +
			" ORDER BY " + rank + " DESC, id ASC LIMIT $" + strconv.Itoa(len(bind)-1) + " OFFSET $" + strconv.Itoa(len(bind))
		rows, err := tx.Query(ctx, stmt, bind...)
		if err != nil {
			return searchError(ctx, filter, err)
		}
		ids := []int64{}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return searchError(ctx, filter, err)
		}
		rows.Close()
		hasMore := len(ids) > limit
		if hasMore {
			ids = ids[:limit]
		}
		out, truncated, err := s.fetchRanked(ctx, tx, n, state, ids, includeHidden)
		if err != nil {
			return err
		}
		result.Rows = out
		result.Truncated = hasMore || truncated
		return nil
	})
	if err != nil {
		return store.SearchResult{}, err
	}
	return result, nil
}

func searchError(ctx context.Context, filter string, err error) error {
	if filter != "" {
		return store.NewFilterError(filter, queryError(ctx, err))
	}
	return queryError(ctx, err)
}

func (s *Store) SearchVector(ctx context.Context, ns, table string, q store.VectorQuery, includeHidden bool, scope *store.RowScope, scopeIncarnation store.Incarnation, page store.Page) (store.SearchResult, error) {
	if page.Offset < 0 {
		return store.SearchResult{}, invalidf("offset must be non-negative")
	}
	if len(q.Args) > 100 {
		return store.SearchResult{}, invalidf("too many filter arguments")
	}
	limit := store.DefaultSearchLimit
	if page.Limit > 0 {
		limit = page.Limit
	}
	if limit > store.MaxSearchLimit {
		limit = store.MaxSearchLimit
	}
	filter := strings.TrimSpace(q.Filter)
	args, err := queryArgs(q.Args)
	if err != nil {
		return store.SearchResult{}, err
	}
	threshold := math.Inf(-1)
	if q.MinScore != nil {
		threshold = *q.MinScore
	}
	result := store.SearchResult{Rows: []map[string]any{}, Execution: store.VectorExact}
	err = s.read(ctx, ns, func(tx pgx.Tx, n namespace) error {
		state, err := s.loadTable(ctx, tx, n, table)
		if err != nil {
			return err
		}
		if err := s.guardScope(ctx, tx, n, table, state, scopeIncarnation); err != nil {
			return err
		}
		if err := scopeUsable(scope, state.schema); err != nil {
			return err
		}
		column, dim, err := store.ResolveVectorColumn(state.schema, table, q.Column, q.EmbedModel != "", q.EmbedModel)
		if err != nil {
			return err
		}
		if dim > 0 && len(q.Vec) != dim {
			return invalidf("query vector has %d entries, column %s expects dim %d", len(q.Vec), column, dim)
		}
		if !store.AllFinite(q.Vec) {
			return invalidf("query vector contains a non-finite component")
		}
		physicalColumn := column
		if column != "_embedding" {
			physicalColumn = state.columns[column]
		}
		physical := ident(n.physical, state.physical)
		stmt := "SELECT id," + ident(physicalColumn) + " FROM " + physical + " WHERE " + ident(physicalColumn) + " IS NOT NULL"
		bind := []any{}
		if filter != "" {
			compiled, bound, err := s.compileMutationFilter(ctx, tx, n, filter, args, state, scope)
			if err != nil {
				return err
			}
			bind = bound
			stmt += " AND id IN (" + compiled + ")"
		}
		if clause, sargs := scopePredicate(scope, "", len(bind)+1); clause != "" {
			stmt += " AND " + clause
			bind = append(bind, sargs...)
		}
		rows, err := tx.Query(ctx, stmt, bind...)
		if err != nil {
			return searchError(ctx, filter, err)
		}
		type hit struct {
			id    int64
			score float64
		}
		hits := []hit{}
		skipped := 0
		for rows.Next() {
			var id int64
			var raw []byte
			if err := rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return err
			}
			stored, err := schema.DecodeVector(raw)
			if err != nil || len(stored) != len(q.Vec) || !store.AllFinite(stored) {
				skipped++
				continue
			}
			score := store.Cosine(q.Vec, stored)
			if score < threshold {
				continue
			}
			hits = append(hits, hit{id: id, score: score})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return searchError(ctx, filter, err)
		}
		rows.Close()
		sort.SliceStable(hits, func(i, j int) bool {
			if hits[i].score == hits[j].score {
				return hits[i].id < hits[j].id
			}
			return hits[i].score > hits[j].score
		})
		offset := page.Offset
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
		out, truncated, err := s.fetchRanked(ctx, tx, n, state, ids, includeHidden)
		if err != nil {
			return err
		}
		for _, row := range out {
			if id, ok := row["id"].(int64); ok {
				row["_score"] = scoreByID[id]
			}
		}
		result.Rows = out
		result.Truncated = hasMore || truncated
		result.SkippedVectors = skipped
		return nil
	})
	if err != nil {
		return store.SearchResult{}, err
	}
	return result, nil
}
