package store

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/lsm/dolmen/internal/schema"
)

// embeddingCol is the hidden column that stores automatic vectorize embeddings.
const embeddingCol = "_embedding"

var embeddingMentionRe = regexp.MustCompile(`(?i)\b_embedding\b`)

// projection is the typed-read contract for one result set: which column
// labels carry a declared field type, the dimension of vector columns, and
// which hidden internal columns to strip. query, search_fulltext, and
// search_vector all shape their rows through it so the three ops cannot drift.
type projection struct {
	types     map[string]schema.FieldType
	dims      map[string]int
	ambiguous map[string]bool
	hidden    map[string]bool
}

func newProjection() *projection {
	return &projection{
		types:     map[string]schema.FieldType{},
		dims:      map[string]int{},
		ambiguous: map[string]bool{},
		hidden:    map[string]bool{embeddingCol: true},
	}
}

// projectionFromSchema builds the projection for reading rows of one known
// table. includeHidden keeps internal columns (currently _embedding) in the
// result instead of stripping them.
func projectionFromSchema(sc *schema.TableSchema, includeHidden bool) *projection {
	p := newProjection()
	if includeHidden {
		p.hidden = nil
	}
	for _, f := range sc.Fields {
		p.types[f.Name] = f.Type
		if f.Type == schema.Vector {
			p.dims[f.Name] = f.Dim
		}
	}
	if sc.VectorizeField() != nil {
		p.types[embeddingCol] = schema.Vector
		p.dims[embeddingCol] = sc.EmbedDim
	}
	return p
}

// addSchema folds one table's fields into a namespace-wide projection for the
// raw-SQL read path, where a statement may reference any table. A field name
// that different tables declare with different types is ambiguous and stays
// uncoerced rather than guessed.
func (p *projection) addSchema(sc *schema.TableSchema) {
	for _, f := range sc.Fields {
		p.addType(f.Name, f.Type)
		if f.Type == schema.Vector {
			p.addDim(f.Name, f.Dim)
		}
	}
	if sc.VectorizeField() != nil {
		p.addType(embeddingCol, schema.Vector)
		p.addDim(embeddingCol, sc.EmbedDim)
	}
}

func (p *projection) addType(name string, t schema.FieldType) {
	if prev, ok := p.types[name]; ok && prev != t {
		p.ambiguous[name] = true
		return
	}
	p.types[name] = t
}

func (p *projection) addDim(name string, dim int) {
	if dim == 0 {
		return
	}
	if prev, ok := p.dims[name]; ok && prev != dim {
		p.dims[name] = 0
		return
	}
	p.dims[name] = dim
}

func (p *projection) fieldType(col string) (schema.FieldType, bool) {
	if p.ambiguous[col] {
		return "", false
	}
	t, ok := p.types[col]
	return t, ok
}

func (p *projection) isHidden(col string) bool {
	return p.hidden != nil && p.hidden[col]
}

// nsProjection builds the namespace-wide projection for a raw SQL statement.
// Referencing _embedding anywhere in the statement opts the hidden column in;
// otherwise it is stripped from results (e.g. from SELECT *).
func (s *Store) nsProjection(ctx context.Context, n *nsDB, statement string) (*projection, error) {
	rows, err := n.ro.QueryContext(ctx, `SELECT schema_json FROM _dolmen_tables`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	p := newProjection()
	if embeddingMentionRe.MatchString(statement) {
		p.hidden = nil
	}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var sc schema.TableSchema
		if err := json.Unmarshal([]byte(raw), &sc); err != nil {
			return nil, fmt.Errorf("corrupt schema in namespace registry: %w", err)
		}
		p.addSchema(&sc)
	}
	return p, rows.Err()
}

// decodeValue coerces one scanned column value to its declared field type:
// boolean -> bool, json -> the decoded value, vector -> []float64. Values that
// do not match the declared storage shape (written outside the typed write
// path) fall back to the raw presentation instead of failing the read.
func decodeValue(t schema.FieldType, v any) any {
	if v == nil {
		return nil
	}
	switch t {
	case schema.Boolean:
		switch b := v.(type) {
		case bool:
			return b
		case int64:
			if b == 0 {
				return false
			}
			if b == 1 {
				return true
			}
		}
		return v
	case schema.JSON:
		s, ok := v.(string)
		if !ok {
			if raw, isBytes := v.([]byte); isBytes {
				s, ok = string(raw), true
			}
		}
		if ok {
			var out any
			dec := json.NewDecoder(strings.NewReader(s))
			dec.UseNumber()
			if err := dec.Decode(&out); err == nil {
				return out
			}
		}
		return v
	case schema.Vector:
		if raw, ok := v.([]byte); ok {
			if fv, err := schema.DecodeVector(raw); err == nil {
				out := make([]float64, len(fv))
				for i, x := range fv {
					out[i] = float64(x)
				}
				return out
			}
		}
		return normalizeVal(v)
	}
	return normalizeVal(v)
}

// decodeColumn applies decodeValue to a result column of this projection.
// Columns without a declared type keep the raw presentation (blobs as base64).
func (p *projection) decodeColumn(col string, v any) any {
	if t, ok := p.fieldType(col); ok {
		return decodeValue(t, v)
	}
	return normalizeVal(v)
}

// presentedSize estimates the encoded size of a column value after typed
// decoding, for the response-budget accounting.
func (p *projection) presentedSize(col string, raw, v any) int {
	t, ok := p.fieldType(col)
	if !ok {
		return approxSize(v)
	}
	switch t {
	case schema.Vector:
		if fv, isFloats := v.([]float64); isFloats {
			if d := p.dims[col]; d > 0 {
				return d*27 + 8
			}
			return len(fv)*27 + 8
		}
	case schema.JSON:
		// Composite values re-encode to about the size of the stored text;
		// approxSize would charge them a flat 16 bytes.
		if _, isStr := v.(string); !isStr {
			return rawValSize(raw)
		}
	}
	return approxSize(v)
}
