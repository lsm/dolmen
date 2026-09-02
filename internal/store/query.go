package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"
)

func normalizeArg(v any) any {
	if n, ok := v.(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			return i
		}
		f, _ := n.Float64()
		return f
	}
	return v
}

var queryStartRe = regexp.MustCompile(`(?i)\A\s*(select|with)\b`)

func (s *Store) Query(ctx context.Context, nsName, query string, args []any) ([]map[string]any, bool, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(query), ";")
	if !queryStartRe.MatchString(strings.TrimSpace(query)) {
		return nil, false, invalidf("only read-only SELECT/WITH statements are allowed")
	}
	if strings.Contains(trimmed, ";") {
		return nil, false, invalidf("multiple statements are not allowed")
	}
	if len(args) > 100 {
		return nil, false, invalidf("too many query parameters")
	}
	for i, a := range args {
		args[i] = normalizeArg(a)
	}
	n, err := s.ns(nsName)
	if err != nil {
		return nil, false, err
	}
	rows, err := n.ro.QueryContext(ctx, trimmed, args...)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil, false, fmt.Errorf("%w: %w", ErrNotFound, err)
		}
		return nil, false, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	defer rows.Close()
	return rowsToMaps(rows)
}

const MaxQueryRows = 1000

const MaxQueryBytes = 32 << 20

func rowsToMaps(rows *sql.Rows) ([]map[string]any, bool, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, false, err
	}
	seen := map[string]bool{}
	labelBytes := 0
	for _, c := range cols {
		if seen[c] {
			return nil, false, invalidf("duplicate column label %q in query result; use AS aliases", c)
		}
		if len(c) > 4096 {
			return nil, false, invalidf("column label exceeds 4096 bytes; use a shorter AS alias")
		}
		seen[c] = true
		labelBytes += encodedSize(c) + 16
	}
	out := []map[string]any{}
	total := 0
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, false, err
		}
		m := make(map[string]any, len(cols))
		rowBytes := 0
		for i, c := range cols {
			if err := checkRowValue(c, vals[i]); err != nil {
				return nil, false, err
			}
			if total+rowBytes+rawValSize(vals[i]) > MaxQueryBytes {
				if len(out) == 0 {
					return nil, false, invalidf("query result exceeds the %d MiB response budget on its first row; select fewer or smaller columns", MaxQueryBytes>>20)
				}
				return out, true, wrapStepErr(rows.Err())
			}
			v := normalizeVal(vals[i])
			m[c] = v
			rowBytes += approxSize(v)
		}
		if len(out) >= MaxQueryRows {
			return out, true, wrapStepErr(rows.Err())
		}
		if total+rowBytes+labelBytes > MaxQueryBytes {
			if len(out) == 0 {
				return nil, false, invalidf("query result exceeds the %d MiB response budget on its first row; select fewer or smaller columns", MaxQueryBytes>>20)
			}
			return out, true, wrapStepErr(rows.Err())
		}
		total += rowBytes + labelBytes
		out = append(out, m)
	}
	return out, false, wrapStepErr(rows.Err())
}

func wrapStepErr(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrInvalid, err)
}

func checkRowValue(col string, v any) error {
	switch t := v.(type) {
	case []byte:
		if len(t) > MaxQueryBytes {
			return invalidf("column %q exceeds the %d MiB response budget; select fewer or smaller columns", col, MaxQueryBytes>>20)
		}
	case float64:
		if math.IsInf(t, 0) || math.IsNaN(t) {
			return invalidf("column %q produced a non-finite value", col)
		}
	}
	return nil
}

func rawValSize(v any) int {
	switch t := v.(type) {
	case []byte:
		return len(t)
	case string:
		return len(t)
	default:
		return 16
	}
}

func approxSize(v any) int {
	switch t := v.(type) {
	case string:
		return encodedSize(t)
	case []byte:
		return len(t)
	default:
		return 16
	}
}

func encodedSize(s string) int {
	n := len(s)
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] < 0x20:
			n += 6
		case s[i] == '"' || s[i] == '\\':
			n += 3
		}
	}
	if strings.Contains(s, "\u2028") || strings.Contains(s, "\u2029") {
		n += 4 * (strings.Count(s, "\u2028") + strings.Count(s, "\u2029"))
	}
	if !utf8.ValidString(s) {
		for _, r := range s {
			if r == utf8.RuneError {
				n += 6
			}
		}
	}
	return n
}

func normalizeVal(v any) any {
	if b, ok := v.([]byte); ok {
		return base64.StdEncoding.EncodeToString(b)
	}
	return v
}
