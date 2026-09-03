package schema

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
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
	return ValidIdent(s) && !strings.Contains(s, "__fts") && !strings.HasPrefix(s, "sqlite_")
}

// cleanName transforms an arbitrary map key into a valid Dolmen field
// identifier. It lowercases, replaces non-identifier characters with
// underscores, ensures the result starts with a letter, and rewrites
// reserved names by appending an underscore.
func cleanName(raw string) string {
	var runes []rune
	for _, r := range strings.ToLower(raw) {
		switch {
		case r >= 'a' && r <= 'z':
			runes = append(runes, r)
		case r >= '0' && r <= '9':
			runes = append(runes, r)
		case r == '_':
			runes = append(runes, r)
		default:
			runes = append(runes, '_')
		}
	}

	if len(runes) > 64 {
		runes = runes[:64]
	}
	if len(runes) == 0 {
		return "x"
	}
	if runes[0] < 'a' || runes[0] > 'z' {
		runes = append([]rune{'x'}, runes...)
		if len(runes) > 64 {
			runes = runes[:64]
		}
	}

	if reserved[string(runes)] {
		if len(runes) < 64 {
			runes = append(runes, '_')
		} else {
			runes[len(runes)-1] = '_'
		}
	}
	return string(runes)
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
		} else if f.Dim != 0 {
			return fmt.Errorf("field %q: dim is only allowed on vector fields", f.Name)
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

func CanonicalTimestamp(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if !isoRe.MatchString(s) {
		return "", false
	}
	norm := s
	if len(norm) >= 11 && norm[10] == 't' {
		b := []byte(norm)
		b[10] = 'T'
		norm = string(b)
	}
	if strings.HasSuffix(norm, "z") {
		norm = norm[:len(norm)-1] + "Z"
	}
	for _, layout := range timestampLayouts {
		if _, err := time.Parse(layout, norm); err == nil {
			if !validRFC3339Offset(s) {
				return "", false
			}
			return norm, true
		}
	}
	return "", false
}

func LooksLikeTimestamp(s string) bool {
	_, ok := CanonicalTimestamp(s)
	return ok
}

var offsetRe = regexp.MustCompile(`(?:[Zz]|[+-]\d{2}:\d{2})$`)

func validRFC3339Offset(s string) bool {
	m := offsetRe.FindStringSubmatch(s)
	if m == nil || m[0] == "Z" || m[0] == "z" {
		return true
	}
	var h, min int
	if _, err := fmt.Sscanf(m[0][1:], "%2d:%2d", &h, &min); err != nil {
		return true
	}
	return h <= 23 && min <= 59
}

// Inference is the full result of running InferSchema. It contains the
// proposed fields, any warnings about sanitized or colliding input keys, and
// a provenance map from each final field name to the original key(s) that
// produced it.
type Inference struct {
	Fields     []Field             `json:"fields"`
	Warnings   []string            `json:"warnings"`
	Provenance map[string][]string `json:"provenance"`
}

// InferSchema inspects sample records and proposes a schema with valid,
// non-colliding field names. It returns warnings and provenance for keys that
// were sanitized or merged due to case/punctuation collisions.
func InferSchema(samples []map[string]any) Inference {
	rawKinds := map[string]map[string]bool{}
	for _, s := range samples {
		for k, v := range s {
			if rawKinds[k] == nil {
				rawKinds[k] = map[string]bool{}
			}
			if isNilValue(v) {
				continue
			}
			rawKinds[k][goKind(v)] = true
		}
	}

	type group struct {
		raws  []string
		kinds map[string]bool
	}
	groups := map[string]*group{}
	for raw, ks := range rawKinds {
		final := cleanName(raw)
		g, ok := groups[final]
		if !ok {
			g = &group{kinds: map[string]bool{}}
			groups[final] = g
		}
		g.raws = append(g.raws, raw)
		for k := range ks {
			g.kinds[k] = true
		}
	}

	finals := make([]string, 0, len(groups))
	for f := range groups {
		finals = append(finals, f)
	}
	sort.Strings(finals)

	var result Inference
	result.Provenance = map[string][]string{}
	for _, final := range finals {
		g := groups[final]
		sort.Strings(g.raws)

		f := Field{Name: final}
		distinct := 0
		for _, present := range g.kinds {
			if present {
				distinct++
			}
		}
		switch {
		case distinct > 1:
			f.Type = JSON
		case g.kinds["bool"]:
			f.Type = Boolean
		case g.kinds["number"]:
			f.Type = Number
		case g.kinds["object"] || g.kinds["array"]:
			f.Type = JSON
		case g.kinds["string"]:
			f.Type = String
			if allStringsMatch(samples, func(k string) bool { return cleanName(k) == final }, LooksLikeTimestamp) {
				f.Type = Timestamp
			} else if allStringsMatch(samples, func(k string) bool { return cleanName(k) == final }, func(s string) bool {
				return len(s) > 200 || strings.ContainsAny(s, "\n")
			}) {
				f.Type = Text
				f.Fulltext = true
			}
		default:
			f.Type = JSON
		}

		result.Fields = append(result.Fields, f)
		result.Provenance[final] = g.raws

		if len(g.raws) > 1 {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("keys %s are variants that collapse to %q; they were merged into field %q", quotedList(g.raws), final, final))
		} else if g.raws[0] != final {
			raw := g.raws[0]
			if reserved[strings.ToLower(raw)] {
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("reserved key %q was renamed to %q", raw, final))
			} else {
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("key %q was sanitized to %q", raw, final))
			}
		}
	}
	return result
}

// InferFields is a convenience wrapper that returns only the proposed fields
// from InferSchema. Callers that need warnings or provenance should use
// InferSchema directly.
func InferFields(samples []map[string]any) []Field {
	return InferSchema(samples).Fields
}

func quotedList(v []string) string {
	q := make([]string, len(v))
	for i, s := range v {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, ", ")
}

func unwrapValue(v any) (rv reflect.Value, nilFound, cycled bool) {
	rv = reflect.ValueOf(v)
	seen := map[uintptr]bool{}
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return rv, true, false
		}
		if rv.Kind() == reflect.Pointer {
			ptr := rv.Pointer()
			if seen[ptr] {
				return rv, false, true
			}
			seen[ptr] = true
		}
		rv = rv.Elem()
	}
	return rv, false, false
}

func isNilValue(v any) bool {
	if v == nil {
		return true
	}
	rv, nilFound, cycled := unwrapValue(v)
	if nilFound || cycled || !rv.IsValid() {
		return true
	}
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Slice, reflect.UnsafePointer:
		return rv.IsNil()
	}
	return false
}

func goKind(v any) string {
	rv, _, cycled := unwrapValue(v)
	if cycled || !rv.IsValid() {
		return "other"
	}
	v = rv.Interface()
	switch v.(type) {
	case bool:
		return "bool"
	case float32, float64, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, uintptr, json.Number:
		return "number"
	case string, time.Time:
		return "string"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		switch reflect.TypeOf(v).Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
			reflect.Float32, reflect.Float64:
			return "number"
		case reflect.String:
			return "string"
		case reflect.Bool:
			return "bool"
		}
		return "other"
	}
}

func allStringsMatch(samples []map[string]any, match func(string) bool, pred func(string) bool) bool {
	for _, s := range samples {
		for k, v := range s {
			if !match(k) || isNilValue(v) {
				continue
			}
			str, ok := underlyingString(v)
			if !ok || !pred(str) {
				return false
			}
		}
	}
	return true
}

func underlyingString(v any) (string, bool) {
	rv, _, cycled := unwrapValue(v)
	if cycled || !rv.IsValid() {
		return "", false
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
