package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
	"github.com/lsm/dolmen/internal/value"
)

func (s *Store) GetRows(ctx context.Context, ns, table string, ids []int64, scope *store.RowScope, expected store.Incarnation) (store.QueryResult, error) {
	if len(ids) > store.MaxReadRowsIDs {
		return store.QueryResult{}, fmt.Errorf("%w: read_rows accepts at most %d ids per request, got %d", store.ErrInvalid, store.MaxReadRowsIDs, len(ids))
	}
	if scope != nil {
		return store.QueryResult{}, derr.New(derr.Forbidden, "PostgreSQL row scopes are not implemented yet")
	}
	result := store.QueryResult{Rows: []map[string]any{}}
	err := s.read(ctx, ns, func(tx pgx.Tx, n namespace) error {
		state, err := s.loadTable(ctx, tx, n, table)
		if err != nil {
			return err
		}
		if err := checkIncarnation(ns, state.incarnation, expected); err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		fields := append([]schema.Field{{Name: "id", Type: schema.Number}, {Name: "created_at", Type: schema.Timestamp}}, state.schema.Fields...)
		columns := make([]string, len(fields))
		labelBytes := 0
		for i, f := range fields {
			physical := f.Name
			if i >= 2 {
				physical = state.columns[f.Name]
			}
			columns[i] = ident(physical)
			if f.Type == schema.Number && i >= 2 {
				columns[i] += "::text"
			}
			labelBytes += value.EncodedSize(f.Name) + 16
		}
		rows, err := tx.Query(ctx, "SELECT "+strings.Join(columns, ",")+" FROM "+ident(n.physical, state.physical)+" WHERE id = ANY($1::bigint[]) ORDER BY id", ids)
		if err != nil {
			return err
		}
		defer rows.Close()
		total := 0
		for rows.Next() {
			raw, err := rows.Values()
			if err != nil {
				return err
			}
			row := make(map[string]any, len(fields))
			size := 0
			for i, f := range fields {
				v := raw[i]
				if f.Type == schema.Number && v != nil && i >= 2 {
					v, err = value.Coerce(f, json.Number(v.(string)))
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
