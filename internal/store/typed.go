package store

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/value"
)

const embeddingCol = "_embedding"

var embeddingMentionRe = regexp.MustCompile(`(?i)\b_embedding\b`)

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

func mentionsEmbedding(stmt string) bool {
	var b strings.Builder
	for i := 0; i < len(stmt); {
		switch c := stmt[i]; c {
		case '\'':
			i++
			for i < len(stmt) {
				if stmt[i] == '\'' {
					if i+1 < len(stmt) && stmt[i+1] == '\'' {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			b.WriteByte(' ')
		case '-':
			if i+1 < len(stmt) && stmt[i+1] == '-' {
				for i < len(stmt) && stmt[i] != '\n' {
					i++
				}
				b.WriteByte(' ')
			} else {
				b.WriteByte(c)
				i++
			}
		case '/':
			if i+1 < len(stmt) && stmt[i+1] == '*' {
				i += 2
				for i+1 < len(stmt) && !(stmt[i] == '*' && stmt[i+1] == '/') {
					i++
				}
				i += 2
				if i > len(stmt) {
					i = len(stmt)
				}
				b.WriteByte(' ')
			} else {
				b.WriteByte(c)
				i++
			}
		default:
			b.WriteByte(c)
			i++
		}
	}
	return embeddingMentionRe.MatchString(b.String())
}

func (s *Store) nsProjection(ctx context.Context, db rowsQuerier, statement string) (*projection, error) {
	rows, err := db.QueryContext(ctx, `SELECT schema_json FROM _dolmen_tables`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	p := newProjection()
	if mentionsEmbedding(statement) {
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

func decodeValue(t schema.FieldType, v any) any {
	return value.Decode(t, v)
}

func (p *projection) decodeColumn(col string, v any) any {
	if t, ok := p.fieldType(col); ok {
		return decodeValue(t, v)
	}
	return normalizeVal(v)
}

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

		if _, isStr := v.(string); !isStr {
			return rawValSize(raw)
		}
	}
	return approxSize(v)
}
