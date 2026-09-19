package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
	"github.com/lsm/dolmen/internal/value"
)

func queryError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) {
		return err
	}
	code := derr.Query
	message := "PostgreSQL query failed; check the SQL and parameter types"
	switch {
	case pgerr.Code == "57014":
		code = derr.Canceled
		message = "PostgreSQL statement timed out; narrow the query or reduce its work"
	case pgerr.Code == "42501":
		code = derr.Forbidden
		message = "PostgreSQL query is not permitted by the namespace role"
	case pgerr.Code == "23505":
		code = derr.Conflict
		message = "PostgreSQL uniqueness conflict"
	case len(pgerr.Code) >= 2 && (pgerr.Code[:2] == "42" || pgerr.Code[:2] == "22" || pgerr.Code[:2] == "23"):
		code = derr.InvalidRequest
		message = "invalid PostgreSQL SQL or value; use describe_table for column names and types and ? for parameters"
	}
	return &derr.Error{Code: code, Message: message + " (SQLSTATE " + pgerr.Code + ")", Cause: err}
}

func queryArgs(args []any) ([]any, error) {
	out := make([]any, len(args))
	for i, arg := range args {
		if number, ok := arg.(json.Number); ok {
			v, err := value.Coerce(schema.Field{Name: "query parameter", Type: schema.Number}, number)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", store.ErrInvalid, err)
			}
			arg = v
		}
		out[i] = arg
	}
	return out, nil
}

func queryProjection(names *sqlNames) map[string]schema.Field {
	fields := map[string]schema.Field{"id": {Name: "id", Type: schema.Number}, "created_at": {Name: "created_at", Type: schema.Timestamp}}
	ambiguous := map[string]bool{}
	for _, table := range names.tables {
		for _, field := range table.schema.Fields {
			if previous, ok := fields[field.Name]; ok && (previous.Type != field.Type || previous.Dim != field.Dim) {
				ambiguous[field.Name] = true
			}
			fields[field.Name] = field
		}
	}
	for name := range ambiguous {
		delete(fields, name)
	}
	return fields
}

func queryRows(rows pgx.Rows, names *sqlNames, limit int) (store.QueryResult, error) {
	result := store.QueryResult{Rows: []map[string]any{}}
	columns := rows.FieldDescriptions()
	labels := make([]string, len(columns))
	seen := map[string]bool{}
	labelBytes := 0
	for i, col := range columns {
		label := names.original(col.Name)
		if seen[label] {
			return result, sqlRejected("duplicate column label %q in query result; use AS aliases", label)
		}
		if len(label) > 4096 {
			return result, sqlRejected("column label exceeds 4096 bytes; use a shorter AS alias")
		}
		seen[label] = true
		labels[i] = label
		labelBytes += value.EncodedSize(label) + 16
	}
	fields := queryProjection(names)
	total := 0
	for rows.Next() {
		if len(result.Rows) == limit {
			result.Truncated = true
			break
		}
		raw, err := rows.Values()
		if err != nil {
			return result, err
		}
		row := map[string]any{}
		size := 0
		for i, label := range labels {
			field := fields[label]
			v := raw[i]
			if (columns[i].DataTypeOID == pgtype.JSONOID || columns[i].DataTypeOID == pgtype.JSONBOID) && v != nil {
				encoded := rows.RawValues()[i]
				if columns[i].DataTypeOID == pgtype.JSONBOID && columns[i].Format == pgx.BinaryFormatCode && len(encoded) > 0 {
					encoded = encoded[1:]
				}
				dec := json.NewDecoder(bytes.NewReader(encoded))
				dec.UseNumber()
				if err := dec.Decode(&v); err != nil {
					return result, err
				}
			}
			switch typed := v.(type) {
			case int16:
				v = int64(typed)
			case int32:
				v = int64(typed)
			case float32:
				v = float64(typed)
			case pgtype.Numeric:
				encoded, err := typed.MarshalJSON()
				if err != nil {
					return result, sqlRejected("column %q produced an invalid numeric value", label)
				}
				v, err = decodeNumber(schema.Field{Name: label, Type: schema.Number}, string(encoded))
				if err != nil {
					return result, sqlRejected("column %q produced a non-finite or out-of-range number", label)
				}
			case time.Time:
				v = typed.Format(time.RFC3339Nano)
			}
			if f, ok := v.(float64); ok && (math.IsNaN(f) || math.IsInf(f, 0)) {
				return result, sqlRejected("column %q produced a non-finite value", label)
			}
			if b, ok := v.([]byte); ok && len(b) > store.MaxQueryBytes {
				return result, sqlRejected("column %q exceeds the %d MiB response budget", label, store.MaxQueryBytes>>20)
			}
			if total+size+value.RawSize(v) > store.MaxQueryBytes {
				if len(result.Rows) == 0 {
					return result, sqlRejected("query result exceeds the %d MiB response budget on its first row; select fewer or smaller columns", store.MaxQueryBytes>>20)
				}
				result.Truncated = true
				return result, nil
			}
			decoded := value.Decode(field.Type, v)
			presented := value.ApproxSize(decoded)
			switch decoded.(type) {
			case nil, string, bool, int64, float64, []byte:
			default:
				encoded, err := json.Marshal(decoded)
				if err != nil {
					return result, sqlRejected("column %q cannot be represented as JSON", label)
				}
				presented = len(encoded)
			}
			if field.Type == schema.Vector {
				if vec, ok := decoded.([]float64); ok {
					presented = len(vec)*27 + 8
				}
			} else if field.Type == schema.JSON {
				if _, ok := decoded.(string); !ok {
					presented = value.RawSize(v)
				}
			}
			size += presented
			if total+size+labelBytes > store.MaxQueryBytes {
				if len(result.Rows) == 0 {
					return result, sqlRejected("query result exceeds the %d MiB response budget on its first row; select fewer or smaller columns", store.MaxQueryBytes>>20)
				}
				result.Truncated = true
				return result, nil
			}
			row[label] = decoded
		}
		total += size + labelBytes
		result.Rows = append(result.Rows, row)
	}
	return result, rows.Err()
}

func (s *Store) Query(ctx context.Context, ns, input string, args []any, expected [16]byte, page store.Page) (store.QueryResult, error) {
	if page.Offset < 0 {
		return store.QueryResult{}, sqlRejected("query offset must not be negative")
	}
	args, err := queryArgs(args)
	if err != nil {
		return store.QueryResult{}, err
	}
	generation, err := s.ensureQueryRole(ctx, ns, expected)
	if err != nil {
		return store.QueryResult{}, err
	}
	result := store.QueryResult{}
	err = s.read(ctx, ns, func(tx pgx.Tx, n namespace) error {
		if n.generation != generation {
			return fmt.Errorf("%w: namespace was replaced", store.ErrNotFound)
		}
		tables, err := s.queryTables(ctx, tx, n)
		if err != nil {
			return err
		}
		sql, names, err := compileSQL(input, len(args), n.physical, tables)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SET TRANSACTION READ ONLY"); err != nil {
			return err
		}
		if err := enterQueryRole(ctx, tx, s.queryRole); err != nil {
			return err
		}
		limit := page.Limit
		if limit <= 0 {
			limit = store.DefaultPageLimit
		}
		if limit > store.MaxPageLimit {
			limit = store.MaxPageLimit
		}
		sql = fmt.Sprintf("SELECT * FROM (%s) AS dolmen_result LIMIT $%d OFFSET $%d", sql, len(args)+1, len(args)+2)
		bind := append(append([]any{}, args...), limit+1, page.Offset)
		rows, err := tx.Query(ctx, sql, bind...)
		if err != nil {
			return queryError(ctx, err)
		}
		defer rows.Close()
		result, err = queryRows(rows, names, limit)
		return queryError(ctx, err)
	})
	if err != nil {
		return store.QueryResult{}, err
	}
	return result, nil
}
