package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/lsm/dolmen/internal/schema"
)

type EmbedFn func(ctx context.Context, texts []string) ([][]float32, error)

type Embedder struct {
	Embed    EmbedFn
	Identity string
}

const MaxRecordsPerInsert = 1000

func (s *Store) Insert(ctx context.Context, nsName, table string, records []map[string]any, emb Embedder) ([]int64, error) {
	if len(records) == 0 {
		return nil, invalidf("no records given")
	}
	if len(records) > MaxRecordsPerInsert {
		return nil, invalidf("too many records: %d > %d per call", len(records), MaxRecordsPerInsert)
	}
	n, err := s.ns(nsName)
	if err != nil {
		return nil, err
	}
	normalized := make([]map[string]any, len(records))
	for i, rec := range records {
		nr := make(map[string]any, len(rec))
		for k, v := range rec {
			lk := strings.ToLower(k)
			if _, exists := nr[lk]; exists {
				return nil, invalidf("record %d: fields %q and its case variant collapse to %q; use one spelling", i, k, lk)
			}
			nr[lk] = v
		}
		normalized[i] = nr
	}
	records = normalized

	for attempt := 0; ; attempt++ {
		if attempt >= 3 {
			return nil, invalidf("table schema changed concurrently; retry the insert")
		}
		ids, done, err := s.insertAttempt(ctx, n, nsName, table, records, emb)
		if done {
			return ids, err
		}
	}
}

func (s *Store) insertAttempt(ctx context.Context, n *nsDB, nsName, table string, records []map[string]any, emb Embedder) (ids []int64, done bool, err error) {
	sc, err := loadSchema(ctx, n.rw, nsName, table)
	if err != nil {
		return nil, true, err
	}
	persistMeta := sc.EmbedSpace == "" || sc.EmbedDim == 0
	origEmbedSpace := sc.EmbedSpace
	origEmbedDim := sc.EmbedDim
	for _, rec := range records {
		for k := range rec {
			if sc.Field(k) == nil {
				return nil, true, invalidf("unknown field %q on table %s (see describe_table)", k, table)
			}
		}
		for _, f := range sc.Fields {
			v, present := rec[f.Name]
			if present && v != nil {
				continue
			}
			if f.Required && (!present || v == nil) {
				return nil, true, invalidf("field %q is required", f.Name)
			}
		}
	}

	type coercedRow struct {
		cols []string
		vals []any
		rec  map[string]any
	}
	coercedRows := make([]coercedRow, len(records))
	for i, rec := range records {
		var cols []string
		var vals []any
		for _, f := range sc.Fields {
			v, present := rec[f.Name]
			if !present {
				continue
			}
			cv, err := coerceValue(f, v)
			if err != nil {
				return nil, true, fmt.Errorf("%w: %w", ErrInvalid, err)
			}
			cols = append(cols, q(f.Name))
			vals = append(vals, cv)
		}
		coercedRows[i] = coercedRow{cols: cols, vals: vals, rec: rec}
	}

	embFor := map[int][]float32{}
	if vf := sc.VectorizeField(); vf != nil {
		var texts []string
		var idx []int
		for i, rec := range records {
			if t, ok := rec[vf.Name].(string); ok && t != "" {
				texts = append(texts, t)
				idx = append(idx, i)
			}
		}
		if len(texts) > 0 {
			if emb.Embed == nil {
				return nil, true, invalidf("table %s uses vectorize but no embedding provider is configured", table)
			}
			if emb.Identity == "" {
				return nil, true, invalidf("table %s uses vectorize but the active embedding provider reports no identity; configure the provider before inserting so rows are attributable to an embedding space", table)
			}
			if sc.EmbedSpace != "" && sc.EmbedSpace != emb.Identity {
				return nil, true, invalidf("embedding provider changed: table rows were embedded by %q but the active provider is %q; re-embed via migrate (set_vectorize off, then on)", sc.EmbedSpace, emb.Identity)
			}
			vecs, err := emb.Embed(ctx, texts)
			if err != nil {
				return nil, true, fmt.Errorf("embedding failed: %w", err)
			}
			if len(vecs) != len(texts) {
				return nil, true, fmt.Errorf("embedding provider returned %d vectors for %d texts", len(vecs), len(texts))
			}
			for _, v := range vecs {
				if len(v) == 0 {
					return nil, true, invalidf("embedding provider returned a zero-dimensional vector for table %s", table)
				}
				for _, x := range v {
					if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
						return nil, true, invalidf("embedding provider returned a non-finite vector component for table %s", table)
					}
				}
				if sc.EmbedDim == 0 {
					sc.EmbedDim = len(v)
				} else if len(v) != sc.EmbedDim {
					return nil, true, invalidf("embedding provider returned %d-dimensional vectors but table %s stores %d-dimensional embeddings; re-embed via migrate (set_vectorize off, then on) if the provider changed", len(v), table, sc.EmbedDim)
				}
			}
			for k, i := range idx {
				embFor[i] = vecs[k]
			}
		}
	}

	fts := sc.FTSFields()
	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		return nil, true, err
	}
	defer tx.Rollback()

	scTx, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return nil, true, err
	}
	if scTx.Version != sc.Version || scTx.EmbedSpace != origEmbedSpace || scTx.EmbedDim != origEmbedDim {
		return nil, false, nil
	}

	ids = make([]int64, 0, len(records))
	for i, row := range coercedRows {
		rec := row.rec
		cols := row.cols
		vals := row.vals
		if ev, ok := embFor[i]; ok {
			cols = append(cols, `"_embedding"`)
			vals = append(vals, schema.EncodeVector(ev))
		}
		var stmt string
		if len(cols) == 0 {
			stmt = fmt.Sprintf(`INSERT INTO %s DEFAULT VALUES`, q(table))
		} else {
			ph := strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",")
			stmt = fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`, q(table), strings.Join(cols, ", "), ph)
		}
		res, err := tx.ExecContext(ctx, stmt, vals...)
		if err != nil {
			return nil, true, fmt.Errorf("insert into %s: %w", table, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, true, err
		}
		ids = append(ids, id)

		if len(fts) > 0 {
			fcols := make([]string, len(fts))
			fvals := make([]any, len(fts)+1)
			fvals[0] = id
			for j, f := range fts {
				fcols[j] = q(f.Name)
				fvals[j+1] = ftsText(rec[f.Name])
			}
			fph := strings.TrimSuffix(strings.Repeat("?,", len(fts)+1), ",")
			fstmt := fmt.Sprintf(`INSERT INTO %s (rowid, %s) VALUES (%s)`,
				q(ftsTable(table)), strings.Join(fcols, ", "), fph)
			if _, err := tx.ExecContext(ctx, fstmt, fvals...); err != nil {
				return nil, true, fmt.Errorf("update search index for %s: %w", table, err)
			}
		}
	}
	if len(embFor) > 0 && persistMeta {
		if sc.EmbedSpace == "" && emb.Identity != "" {
			sc.EmbedSpace = emb.Identity
		}
		if sc.EmbedDim == 0 {
			for _, v := range embFor {
				sc.EmbedDim = len(v)
				break
			}
		}
		raw, err := json.Marshal(sc)
		if err != nil {
			return nil, true, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE _dolmen_tables SET schema_json = ? WHERE name = ?`, string(raw), table); err != nil {
			return nil, true, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, true, err
	}
	return ids, true, nil
}

func ftsText(v any) any {
	if v == nil {
		return nil
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func coerceValue(f schema.Field, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch f.Type {
	case schema.Number:
		switch n := v.(type) {
		case float64:
			return n, nil
		case float32:
			return float64(n), nil
		case int:
			return int64(n), nil
		case int8:
			return int64(n), nil
		case int16:
			return int64(n), nil
		case int32:
			return int64(n), nil
		case int64:
			return n, nil
		case uint:
			if uint64(n) > math.MaxInt64 {
				return nil, fmt.Errorf("field %q: number overflows int64", f.Name)
			}
			return int64(n), nil
		case uint8:
			return int64(n), nil
		case uint16:
			return int64(n), nil
		case uint32:
			return int64(n), nil
		case uint64, uintptr:
			u := reflect.ValueOf(v).Uint()
			if u > math.MaxInt64 {
				return nil, fmt.Errorf("field %q: number overflows int64", f.Name)
			}
			return int64(u), nil
		case json.Number:
			if i, err := n.Int64(); err == nil {
				return i, nil
			}
			fErrName := f.Name
			f, err := n.Float64()
			if err != nil {
				return nil, fmt.Errorf("field %q: expected a number", fErrName)
			}
			return f, nil
		default:
			rv := reflect.ValueOf(v)
			switch rv.Kind() {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				return rv.Int(), nil
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
				u := rv.Uint()
				if u > math.MaxInt64 {
					return nil, fmt.Errorf("field %q: number overflows int64", f.Name)
				}
				return int64(u), nil
			case reflect.Float32, reflect.Float64:
				return rv.Float(), nil
			}
			return nil, fmt.Errorf("field %q: expected a number", f.Name)
		}
	case schema.Boolean:
		b, ok := v.(bool)
		if !ok {
			rv := reflect.ValueOf(v)
			if rv.Kind() != reflect.Bool {
				return nil, fmt.Errorf("field %q: expected a boolean", f.Name)
			}
			b = rv.Bool()
		}
		if b {
			return int64(1), nil
		}
		return int64(0), nil
	case schema.Vector:
		var floats []float64
		switch arr := v.(type) {
		case []any:
			floats = make([]float64, len(arr))
			for i, x := range arr {
				switch n := x.(type) {
				case float64:
					floats[i] = n
				case int:
					floats[i] = float64(n)
				case int64:
					floats[i] = float64(n)
				case json.Number:
					fv, err := n.Float64()
					if err != nil {
						return nil, fmt.Errorf("field %q: vector entries must be numbers", f.Name)
					}
					floats[i] = fv
				default:
					return nil, fmt.Errorf("field %q: vector entries must be numbers", f.Name)
				}
			}
		case []float64:
			floats = arr
		case []float32:
			floats = make([]float64, len(arr))
			for i, x := range arr {
				floats[i] = float64(x)
			}
		default:
			return nil, fmt.Errorf("field %q: expected an array of numbers", f.Name)
		}
		if len(floats) != f.Dim {
			return nil, fmt.Errorf("field %q: vector has %d entries, expected dim %d", f.Name, len(floats), f.Dim)
		}
		out := make([]float32, len(floats))
		for i, x := range floats {
			if math.IsNaN(x) || math.Abs(x) > math.MaxFloat32 {
				return nil, fmt.Errorf("field %q: vector entry %d is outside the float32 range", f.Name, i)
			}
			out[i] = float32(x)
		}
		return schema.EncodeVector(out), nil
	case schema.JSON:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("field %q: cannot marshal JSON: %w", f.Name, err)
		}
		return string(b), nil
	default:
		s, ok := storedString(v)
		if !ok {
			return nil, fmt.Errorf("field %q: expected a string", f.Name)
		}
		return s, nil
	}
}

func storedString(v any) (string, bool) {
	rv := reflect.ValueOf(v)
	seen := map[uintptr]bool{}
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return "", false
		}
		if rv.Kind() == reflect.Pointer {
			ptr := rv.Pointer()
			if seen[ptr] {
				return "", false
			}
			seen[ptr] = true
		}
		rv = rv.Elem()
	}
	if rv.Kind() == reflect.Struct {
		if t, ok := rv.Interface().(time.Time); ok {
			return t.Format(time.RFC3339Nano), true
		}
		return "", false
	}
	if rv.Kind() != reflect.String {
		return "", false
	}
	return rv.String(), true
}
