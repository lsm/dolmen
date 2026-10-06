package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
	"github.com/lsm/dolmen/internal/value"
)

type rowSelection struct {
	ids     []int64
	paged   bool
	afterID int64
	limit   int
}

func (s *Store) GetRows(ctx context.Context, ns, table string, ids []int64, scope *store.RowScope, expected store.Incarnation) (_ store.QueryResult, err error) {
	ctx, end := s.span(ctx, "SELECT", ns, table)
	defer func() { end(err) }()
	if len(ids) > store.MaxReadRowsIDs {
		return store.QueryResult{}, fmt.Errorf("%w: read_rows accepts at most %d ids per request, got %d", store.ErrInvalid, store.MaxReadRowsIDs, len(ids))
	}
	return s.readRows(ctx, ns, table, rowSelection{ids: ids}, scope, expected)
}

func (s *Store) ListRows(ctx context.Context, ns, table string, afterID int64, limit int, scope *store.RowScope, expected store.Incarnation) (_ store.QueryResult, err error) {
	ctx, end := s.span(ctx, "SELECT", ns, table)
	defer func() { end(err) }()
	if err := store.ValidateRowPage(afterID, limit); err != nil {
		return store.QueryResult{}, err
	}
	return s.readRows(ctx, ns, table, rowSelection{paged: true, afterID: afterID, limit: limit}, scope, expected)
}

func (s *Store) readRows(ctx context.Context, ns, table string, sel rowSelection, scope *store.RowScope, expected store.Incarnation) (store.QueryResult, error) {
	result := store.QueryResult{Rows: []map[string]any{}}
	err := s.read(ctx, ns, func(tx pgx.Tx, n namespace) error {
		state, err := s.loadTable(ctx, tx, n, table)
		if err != nil {
			return err
		}
		if err := s.guardScope(ctx, tx, n, table, state, expected); err != nil {
			return err
		}
		if err := scopeUsable(scope, state.schema); err != nil {
			return err
		}
		reveal, err := s.revealSet(ctx, state.schema)
		if err != nil {
			return err
		}
		if !sel.paged && len(sel.ids) == 0 {
			return nil
		}
		fields := append([]schema.Field{{Name: "id", Type: schema.Number}, {Name: "created_at", Type: schema.Timestamp}}, state.schema.Fields...)
		if state.schema.HasOwner {
			fields = append(fields, schema.Field{Name: schema.OwnerColumn, Type: schema.Text})
		}
		columns := make([]string, len(fields))
		labelBytes := 0
		for i, f := range fields {
			physical := f.Name
			if i >= 2 && f.Name != schema.OwnerColumn {
				physical = state.columns[f.Name]
			}
			columns[i] = ident(physical)
			if f.Type == schema.Number && i >= 2 && f.Name != schema.OwnerColumn {
				columns[i] += "::text"
			}
			labelBytes += value.EncodedSize(f.Name) + 16
		}
		stmt := "SELECT " + strings.Join(columns, ",") + " FROM " + ident(n.physical, state.physical) + " WHERE id = ANY($1::bigint[])"
		args := []any{sel.ids}
		if sel.paged {
			stmt = "SELECT " + strings.Join(columns, ",") + " FROM " + ident(n.physical, state.physical) + " WHERE id > $1"
			args = []any{sel.afterID}
		}
		if clause, sargs := scopePredicate(scope, "", len(args)+1); clause != "" {
			stmt += " AND " + clause
			args = append(args, sargs...)
		}
		stmt += " ORDER BY id"
		if sel.paged {
			stmt += fmt.Sprintf(" LIMIT %d", sel.limit+1)
		}
		rows, err := tx.Query(ctx, stmt, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		total := 0
		for rows.Next() {
			if sel.paged && len(result.Rows) == sel.limit {
				result.Truncated = true
				return nil
			}
			raw, err := rows.Values()
			if err != nil {
				return err
			}
			row := make(map[string]any, len(fields))
			size := 0
			for i, f := range fields {
				v := raw[i]
				if f.Type == schema.Number && v != nil && i >= 2 {
					v, err = decodeNumber(f, v.(string))
					if err != nil {
						return fmt.Errorf("%w: column %q contains an invalid number", store.ErrInvalid, f.Name)
					}
				}
				if b, ok := v.([]byte); ok && len(b) > store.MaxQueryBytes {
					return fmt.Errorf("%w: column %q exceeds the %d MiB response budget; select fewer or smaller columns", store.ErrInvalid, f.Name, store.MaxQueryBytes>>20)
				}
				if total+size+value.RawSize(v) > store.MaxQueryBytes {
					if len(result.Rows) == 0 {
						return fmt.Errorf("%w: search result exceeds the %d MiB response budget on its first row", store.ErrInvalid, store.MaxQueryBytes>>20)
					}
					result.Truncated = true
					return nil
				}
				decoded := value.Decode(f.Type, v)
				if plain, ok, err := s.presentSecret(reveal, f, v); err != nil {
					return err
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
				if total+size+labelBytes > store.MaxQueryBytes {
					if len(result.Rows) == 0 {
						return fmt.Errorf("%w: search result exceeds the %d MiB response budget on its first row", store.ErrInvalid, store.MaxQueryBytes>>20)
					}
					result.Truncated = true
					return nil
				}
				row[f.Name] = decoded
			}
			total += size + labelBytes
			result.Rows = append(result.Rows, row)
		}
		return rows.Err()
	})
	if err != nil {
		return store.QueryResult{}, err
	}
	return result, nil
}
