package lakehouse

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/iceberg-go"
	"github.com/apache/iceberg-go/table"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
	"github.com/lsm/dolmen/internal/value"
)

func cellValue(arr arrow.Array, i int) any {
	if arr.IsNull(i) {
		return nil
	}
	switch a := arr.(type) {
	case *array.Int64:
		return a.Value(i)
	case *array.Int32:
		return int64(a.Value(i))
	case *array.Boolean:
		return a.Value(i)
	case *array.String:
		return a.Value(i)
	case *array.LargeString:
		return a.Value(i)
	case *array.Binary:
		return slices.Clone(a.Value(i))
	case *array.LargeBinary:
		return slices.Clone(a.Value(i))
	case *array.Float64:
		return a.Value(i)
	}
	return arr.ValueStr(i)
}

func scanRows(ctx context.Context, state tableState, filter iceberg.BooleanExpression) ([]map[string]any, error) {
	if state.native.Metadata().CurrentSnapshot() == nil {
		return nil, nil
	}
	opts := []table.ScanOption{}
	if filter != nil {
		opts = append(opts, table.WithRowFilter(filter))
	}
	_, records, err := state.native.Scan(opts...).ToArrowRecords(ctx)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for rec, err := range records {
		if err != nil {
			return nil, err
		}
		sch := rec.Schema()
		for r := 0; r < int(rec.NumRows()); r++ {
			row := make(map[string]any, int(rec.NumCols()))
			for c := 0; c < int(rec.NumCols()); c++ {
				row[sch.Field(c).Name] = cellValue(rec.Column(c), r)
			}
			out = append(out, row)
		}
		rec.Release()
	}
	slices.SortFunc(out, func(a, b map[string]any) int {
		x, y := a["id"].(int64), b["id"].(int64)
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	})
	return out, nil
}

func decodeNumber(f schema.Field, raw string) (any, error) {
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return n, nil
	}
	x, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return value.Coerce(f, json.Number(raw))
	}
	if x == math.Trunc(x) && x >= -(1<<63) && x < 1<<63 {
		return int64(x), nil
	}
	return x, nil
}

func inScope(sc *schema.TableSchema, scope *store.RowScope, row map[string]any) bool {
	if scope == nil {
		return true
	}
	if scope.Empty {
		return false
	}
	if scope.Owner == "" || !sc.HasOwner {
		return true
	}
	return row[schema.OwnerColumn] == scope.Owner
}

func (s *Store) presentRow(sc *schema.TableSchema, reveal map[string]bool, sealed map[string][]byte, raw map[string]any, hidden bool) (map[string]any, int, error) {
	fields := append([]schema.Field{{Name: "id", Type: schema.Number}, {Name: "created_at", Type: schema.Timestamp}}, sc.Fields...)
	if sc.HasOwner {
		fields = append(fields, schema.Field{Name: schema.OwnerColumn, Type: schema.Text})
	}
	if hidden && sc.VectorizeField() != nil {
		fields = append(fields, schema.Field{Name: "_embedding", Type: schema.Vector})
	}
	row := make(map[string]any, len(fields))
	size := 0
	for i, f := range fields {
		v := raw[f.Name]
		if f.Type == schema.Number && i >= 2 && v != nil {
			text, ok := v.(string)
			if !ok {
				return nil, 0, fmt.Errorf("%w: column %q holds %T where a number's text is stored", store.ErrCatalogCorrupt, f.Name, v)
			}
			number, err := decodeNumber(f, text)
			if err != nil {
				return nil, 0, fmt.Errorf("%w: column %q contains an invalid number", store.ErrCatalogCorrupt, f.Name)
			}
			v = number
		}
		decoded := value.Decode(f.Type, v)
		if f.Type == schema.Secret && v != nil && reveal[f.Name] {
			stored, ok := sealed[f.Name]
			if !ok {
				return nil, 0, fmt.Errorf("%w: secret %q of a row is missing from the catalog", store.ErrCatalogCorrupt, f.Name)
			}
			plain, err := store.OpenSecret(s.secrets, f.Name, stored)
			if err != nil {
				return nil, 0, err
			}
			decoded = plain
		}
		size += value.EncodedSize(f.Name) + 16 + presentedSize(f, v, decoded)
		row[f.Name] = decoded
	}
	return row, size, nil
}

func (s *Store) GetRows(ctx context.Context, ns, name string, ids []int64, scope *store.RowScope, expected store.Incarnation) (store.QueryResult, error) {
	if len(ids) > store.MaxReadRowsIDs {
		return store.QueryResult{}, invalidf("read_rows accepts at most %d ids per request, got %d", store.MaxReadRowsIDs, len(ids))
	}
	return s.readRows(ctx, ns, name, scope, expected, func(state tableState) ([]map[string]any, bool, error) {
		if len(ids) == 0 {
			return nil, false, nil
		}
		raws, err := scanRows(ctx, state, idFilter(ids))
		return raws, false, err
	})
}

func (s *Store) ListRows(ctx context.Context, ns, name string, afterID int64, limit int, scope *store.RowScope, expected store.Incarnation) (store.QueryResult, error) {
	if err := store.ValidateRowPage(afterID, limit); err != nil {
		return store.QueryResult{}, err
	}
	return s.readRows(ctx, ns, name, scope, expected, func(state tableState) ([]map[string]any, bool, error) {
		raws, err := scanRows(ctx, state, iceberg.GreaterThan(iceberg.Reference("id"), afterID))
		if err != nil {
			return nil, false, err
		}
		page := make([]map[string]any, 0, min(len(raws), limit))
		for _, raw := range raws {
			if !inScope(state.schema, scope, raw) {
				continue
			}
			if len(page) == limit {
				return page, true, nil
			}
			page = append(page, raw)
		}
		return page, false, nil
	})
}

func (s *Store) readRows(ctx context.Context, ns, name string, scope *store.RowScope, expected store.Incarnation, pick func(tableState) ([]map[string]any, bool, error)) (store.QueryResult, error) {
	result := store.QueryResult{Rows: []map[string]any{}}
	err := s.withNamespace(ctx, ns, func(n *namespace) error {
		state, err := loadTable(ctx, n, ns, name)
		if err != nil {
			return err
		}
		if err := checkScopeExpected(state, expected); err != nil {
			return err
		}
		if scope != nil && !scope.Empty && !state.schema.HasOwner {
			return invalidf("table %s carries no owner column, so a row scope cannot be applied to it", name)
		}
		reveal, err := store.RevealSet(ctx, state.schema, s.secrets)
		if err != nil {
			return err
		}
		raws, more, err := pick(state)
		if err != nil || len(raws) == 0 {
			return err
		}
		result.Truncated = more
		sealed, err := loadSecrets(ctx, n, state, reveal, raws)
		if err != nil {
			return err
		}
		total := 0
		for _, raw := range raws {
			if !inScope(state.schema, scope, raw) {
				continue
			}
			row, size, err := s.presentRow(state.schema, reveal, sealed[raw["id"].(int64)], raw, false)
			if err != nil {
				return err
			}
			if total+size > store.MaxQueryBytes {
				if len(result.Rows) == 0 {
					return invalidf("search result exceeds the %d MiB response budget on its first row", store.MaxQueryBytes>>20)
				}
				result.Truncated = true
				return nil
			}
			total += size
			result.Rows = append(result.Rows, row)
		}
		return nil
	})
	if err != nil {
		return store.QueryResult{}, err
	}
	return result, nil
}

func loadSecrets(ctx context.Context, n *namespace, state tableState, reveal map[string]bool, raws []map[string]any) (map[int64]map[string][]byte, error) {
	out := map[int64]map[string][]byte{}
	if len(reveal) == 0 || len(raws) == 0 {
		return out, nil
	}
	args := []any{state.incarnation.Table, state.incarnation.DropGen}
	wanted := make(map[int64]bool, len(raws))
	for _, raw := range raws {
		id := raw["id"].(int64)
		wanted[id] = true
		args = append(args, id)
	}
	rows, err := n.db.QueryContext(ctx, `SELECT row_id, field, value FROM _dolmen_lakehouse_secrets WHERE table_name = ? AND generation = ? AND row_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(raws)), ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var field string
		var value []byte
		if err := rows.Scan(&id, &field, &value); err != nil {
			return nil, err
		}
		if !wanted[id] || !reveal[field] {
			continue
		}
		if out[id] == nil {
			out[id] = map[string][]byte{}
		}
		out[id][field] = value
	}
	return out, rows.Err()
}

func presentedSize(f schema.Field, raw, decoded any) int {
	switch f.Type {
	case schema.Vector:
		if floats, ok := decoded.([]float64); ok {
			if f.Dim > 0 {
				return f.Dim*27 + 8
			}
			return len(floats)*27 + 8
		}
	case schema.JSON:
		if _, ok := decoded.(string); !ok {
			return value.RawSize(raw)
		}
	}
	return value.ApproxSize(decoded)
}
