package schema

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

type FieldType string

const (
	String    FieldType = "string"
	Text      FieldType = "text"
	Number    FieldType = "number"
	Boolean   FieldType = "boolean"
	Timestamp FieldType = "timestamp"
	JSON      FieldType = "json"
	Vector    FieldType = "vector"
)

const MaxVectorDim = 4096

type Field struct {
	Name      string    `json:"name"`
	Type      FieldType `json:"type"`
	Fulltext  bool      `json:"fulltext,omitempty"`
	Vectorize bool      `json:"vectorize,omitempty"`
	Dim       int       `json:"dim,omitempty"`
	Required  bool      `json:"required,omitempty"`
}

type TableSchema struct {
	Namespace  string  `json:"namespace"`
	Name       string  `json:"name"`
	Version    int     `json:"version"`
	Fields     []Field `json:"fields"`
	EmbedSpace string  `json:"embed_space,omitempty"`
	EmbedDim   int     `json:"embed_dim,omitempty"`
}

type Change struct {
	Op    string `json:"op"`
	Field *Field `json:"field,omitempty"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
	Name  string `json:"name,omitempty"`
	Value bool   `json:"value,omitempty"`
}

const (
	OpAddField     = "add_field"
	OpRenameField  = "rename_field"
	OpDropField    = "drop_field"
	OpSetFulltext  = "set_fulltext"
	OpSetVectorize = "set_vectorize"
)

var identRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

var reserved = map[string]bool{
	"id":         true,
	"created_at": true,
	"_embedding": true,
	"_score":     true,
	"_rank":      true,
	"rowid":      true,
}

func ValidIdent(s string) bool {
	return identRe.MatchString(s) && !reserved[s]
}

func ValidTableName(s string) bool {
	return ValidIdent(s) && !strings.Contains(s, "__fts")
}

func Normalize(fields []Field) []Field {
	out := make([]Field, len(fields))
	copy(out, fields)
	for i := range out {
		if out[i].Type == "" {
			out[i].Type = String
		}
	}
	return out
}

func Validate(fields []Field) error {
	if len(fields) == 0 {
		return fmt.Errorf("table needs at least one field")
	}
	seen := map[string]bool{}
	vectorizeCount := 0
	for _, f := range fields {
		if !ValidIdent(f.Name) {
			return fmt.Errorf("invalid field name %q: must match ^[a-z][a-z0-9_]{0,63}$ and not be reserved", f.Name)
		}
		if seen[f.Name] {
			return fmt.Errorf("duplicate field name %q", f.Name)
		}
		seen[f.Name] = true
		switch f.Type {
		case String, Text, Number, Boolean, Timestamp, JSON, Vector:
		default:
			return fmt.Errorf("field %q: unknown type %q (valid: string, text, number, boolean, timestamp, json, vector)", f.Name, f.Type)
		}
		if f.Fulltext && f.Type != String && f.Type != Text {
			return fmt.Errorf("field %q: fulltext is only allowed on string or text fields", f.Name)
		}
		if f.Fulltext && f.Name == "rank" {
			return fmt.Errorf("field %q: rank cannot be a fulltext field (reserved by the FTS5 index)", f.Name)
		}
		if f.Vectorize {
			if f.Type != String && f.Type != Text {
				return fmt.Errorf("field %q: vectorize is only allowed on string or text fields", f.Name)
			}
			vectorizeCount++
			if vectorizeCount > 1 {
				return fmt.Errorf("at most one field may be vectorized per table")
			}
		}
		if f.Type == Vector {
			if f.Dim < 1 || f.Dim > MaxVectorDim {
				return fmt.Errorf("field %q: vector fields need dim between 1 and %d", f.Name, MaxVectorDim)
			}
		}
	}
	return nil
}

func (t TableSchema) Field(name string) *Field {
	for i := range t.Fields {
		if t.Fields[i].Name == name {
			return &t.Fields[i]
		}
	}
	return nil
}

func (t TableSchema) FTSFields() []Field {
	var out []Field
	for _, f := range t.Fields {
		if f.Fulltext {
			out = append(out, f)
		}
	}
	return out
}

func (t TableSchema) VectorizeField() *Field {
	for i := range t.Fields {
		if t.Fields[i].Vectorize {
			return &t.Fields[i]
		}
	}
	return nil
}

func (t TableSchema) VectorFields() []Field {
	var out []Field
	for _, f := range t.Fields {
		if f.Type == Vector {
			out = append(out, f)
		}
	}
	return out
}

func SQLType(f Field) string {
	switch f.Type {
	case Number:
		return "NUMERIC"
	case Boolean:
		return "INTEGER"
	case Vector:
		return "BLOB"
	default:
		return "TEXT"
	}
}

func EncodeVector(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(x))
	}
	return b
}

func DecodeVector(b []byte) ([]float32, error) {
	if len(b)%4 != 0 {
		return nil, fmt.Errorf("blob length %d is not a multiple of 4", len(b))
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out, nil
}

var isoRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}([Tt ][0-9:.+\-Zz]+)?$`)

var timestampLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

func looksLikeTimestamp(s string) bool {
	s = strings.TrimSpace(s)
	if !isoRe.MatchString(s) {
		return false
	}
	for _, layout := range timestampLayouts {
		if _, err := time.Parse(layout, s); err == nil {
			return true
		}
	}
	return false
}

func InferFields(samples []map[string]any) []Field {
	kinds := map[string]map[string]bool{}
	for _, s := range samples {
		for k, v := range s {
			lk := strings.ToLower(k)
			if kinds[lk] == nil {
				kinds[lk] = map[string]bool{}
			}
			if v == nil {
				continue
			}
			kinds[lk][goKind(v)] = true
		}
	}
	keys := make([]string, 0, len(kinds))
	for k := range kinds {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var fields []Field
	for _, k := range keys {
		f := Field{Name: strings.ToLower(k)}
		ks := kinds[k]
		distinct := 0
		for _, present := range ks {
			if present {
				distinct++
			}
		}
		switch {
		case distinct > 1:
			f.Type = JSON
		case ks["bool"]:
			f.Type = Boolean
		case ks["number"]:
			f.Type = Number
		case ks["object"] || ks["array"]:
			f.Type = JSON
		case ks["string"]:
			f.Type = String
			if allStringsMatch(samples, k, looksLikeTimestamp) {
				f.Type = Timestamp
			} else if allStringsMatch(samples, k, func(s string) bool {
				return len(s) > 200 || strings.ContainsAny(s, "\n")
			}) {
				f.Type = Text
				f.Fulltext = true
			}
		default:
			f.Type = JSON
		}
		fields = append(fields, f)
	}
	return fields
}

func goKind(v any) string {
	switch v.(type) {
	case bool:
		return "bool"
	case float64, int, int64, json.Number:
		return "number"
	case string:
		return "string"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		return "other"
	}
}

func allStringsMatch(samples []map[string]any, key string, pred func(string) bool) bool {
	for _, s := range samples {
		for k, v := range s {
			if strings.ToLower(k) != key || v == nil {
				continue
			}
			str, ok := v.(string)
			if !ok || !pred(str) {
				return false
			}
		}
	}
	return true
}
