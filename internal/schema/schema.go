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
	Secret    FieldType = "secret"
)

const MaxVectorDim = 4096

const NowDefault = "now()"

type Field struct {
	Name      string    `json:"name"`
	Type      FieldType `json:"type"`
	Fulltext  bool      `json:"fulltext,omitempty"`
	Vectorize bool      `json:"vectorize,omitempty"`
	Dim       int       `json:"dim,omitempty"`
	Required  bool      `json:"required,omitempty"`

	Enum []string `json:"enum,omitempty"`

	Default any `json:"default,omitempty"`
}

type TableSchema struct {
	Namespace  string  `json:"namespace"`
	Name       string  `json:"name"`
	Version    int     `json:"version"`
	Fields     []Field `json:"fields"`
	EmbedSpace string  `json:"embed_space,omitempty"`
	EmbedDim   int     `json:"embed_dim,omitempty"`
	RowAccess  string  `json:"row_access,omitempty"`
	HasOwner   bool    `json:"has_owner,omitempty"`
}

const (
	RowAccessOwn = "own"
	OwnerColumn  = "owner"
)

func ValidateRowAccess(v string) error {
	if v == "" || v == RowAccessOwn {
		return nil
	}
	return fmt.Errorf("row_access must be %q, the only value this version defines; omit the key for a table whose rows every grant holder can see", RowAccessOwn)
}

func ReservedWithOwner(name string) bool {
	return name == OwnerColumn
}

type Change struct {
	Op    string `json:"op"`
	Field *Field `json:"field,omitempty"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
	Name  string `json:"name,omitempty"`

	Value *bool `json:"value,omitempty"`

	Enum *[]string `json:"enum,omitempty"`

	Default any `json:"default,omitempty"`
}

const (
	OpAddField     = "add_field"
	OpRenameField  = "rename_field"
	OpDropField    = "drop_field"
	OpSetFulltext  = "set_fulltext"
	OpSetVectorize = "set_vectorize"
	OpSetEnum      = "set_enum"
	OpSetRowAccess = "set_row_access"
)

func TakesValue(op string) bool {
	switch op {
	case OpSetFulltext, OpSetVectorize, OpSetRowAccess:
		return true
	}
	return false
}

func (c Change) ReadsRows() bool {
	switch c.Op {
	case OpAddField:
		return c.Field == nil || c.Field.Required || c.Field.Fulltext || c.Field.Vectorize || c.Default != nil
	case OpSetEnum, OpSetVectorize, OpSetFulltext, OpSetRowAccess, OpDropField:
		return true
	}
	return false
}

var identRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

const ScoreColumn = "_score"

var reserved = map[string]bool{
	"id":         true,
	"created_at": true,
	"_embedding": true,
	"_score":     true,
	"_rank":      true,
	"rowid":      true,
}

var forbidden map[string]bool

var reservedSuggestions = map[string]string{
	"id":         "record_id",
	"created_at": "created_time",
	"_embedding": "embedding",
	"_score":     "score",
	"_rank":      "rank",
	"rowid":      "record_id",
}

func init() {
	forbidden = make(map[string]bool, len(reserved)+len(sqlKeywords))
	for k := range reserved {
		forbidden[k] = true
	}
	for k := range sqlKeywords {
		forbidden[k] = true
	}
}

func ValidIdent(s string) bool {
	return identRe.MatchString(s) && !forbidden[s]
}

func ValidIdentSyntax(s string) bool {
	return identRe.MatchString(s)
}

func ValidTableName(s string) bool {
	return ValidateTableName(s) == nil
}

func ValidateEnum(field string, vals []string) error {
	if len(vals) == 0 {
		return fmt.Errorf("field %q: enum must list at least one value (pass an empty list to set_enum only, which removes the constraint)", field)
	}
	seen := make(map[string]bool, len(vals))
	for _, v := range vals {
		if v == "" {
			return fmt.Errorf("field %q: enum values must not be empty strings", field)
		}
		if seen[v] {
			return fmt.Errorf("field %q: enum lists duplicate value %q", field, v)
		}
		seen[v] = true
	}
	return nil
}

func EnumAllows(enum []string, s string) bool {
	if len(enum) == 0 {
		return true
	}
	for _, v := range enum {
		if v == s {
			return true
		}
	}
	return false
}

func IsNowDefault(v any) bool {
	s, ok := v.(string)
	return ok && s == NowDefault
}

func ValidateIdent(name, what string) error {
	if !identRe.MatchString(name) {
		return fmt.Errorf("invalid %s %q: must start with a lowercase letter, contain only a-z, 0-9, and underscores, and be at most 64 characters", what, name)
	}
	if reserved[name] {
		return fmt.Errorf("%s %q is a reserved internal name; use a different name such as %q", what, name, SuggestIdent(name))
	}
	if sqlKeywords[name] {
		return fmt.Errorf("%s %q is a reserved SQLite/SQL keyword; use a different name such as %q", what, name, SuggestIdent(name))
	}
	return nil
}

func ValidateTableName(name string) error {
	if err := ValidateIdent(name, "table name"); err != nil {
		return err
	}
	if strings.Contains(name, "__fts") {
		return fmt.Errorf("table name %q must not contain __fts (reserved for full-text search indexes)", name)
	}
	if strings.HasPrefix(name, "sqlite_") {
		return fmt.Errorf("table name %q must not start with sqlite_ (reserved for SQLite internal objects)", name)
	}
	if strings.HasPrefix(name, "pragma_") {
		return fmt.Errorf("table name %q must not start with pragma_ (reserved for SQLite pragma virtual tables)", name)
	}
	if name == "dbstat" {
		return fmt.Errorf("table name %q is reserved for the SQLite dbstat virtual table", name)
	}
	return nil
}

func SuggestIdent(name string) string {
	if ValidIdent(name) {
		return name
	}
	if s, ok := reservedSuggestions[name]; ok && ValidIdent(s) {
		return s
	}
	for _, cand := range []string{"my_" + name, name + "_field"} {
		if ValidIdent(cand) {
			return cand
		}
	}
	return "a different name"
}

func IdentPattern() string {
	names := make([]string, 0, len(forbidden))
	for k := range forbidden {
		names = append(names, regexp.QuoteMeta(k))
	}
	sort.Strings(names)
	return `^(?!(?:` + strings.Join(names, "|") + `)$)[a-z][a-z0-9_]{0,63}$`
}

func ReservedFieldNames() []string {
	return []string{"id", "created_at", "_embedding", "_score", "_rank", "rowid"}
}

func ReservedTableNames() []string {
	return []string{"id", "created_at", "rowid", "dbstat"}

}

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

	if forbidden[string(runes)] {
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
	return validate(fields, nil)
}

func ValidateForMigration(fields, existing []Field) error {
	legacy := make(map[string]Field, len(existing))
	for _, f := range existing {
		legacy[f.Name] = f
	}
	return validate(fields, legacy)
}

func validate(fields []Field, legacy map[string]Field) error {
	if len(fields) == 0 {
		return fmt.Errorf("table needs at least one field")
	}
	seen := map[string]bool{}
	vectorizeCount := 0
	for _, f := range fields {
		if _, carried := legacy[f.Name]; !carried {
			if err := ValidateIdent(f.Name, "field name"); err != nil {
				return err
			}
		}
		if seen[f.Name] {
			return fmt.Errorf("duplicate field name %q", f.Name)
		}
		seen[f.Name] = true
		switch f.Type {
		case String, Text, Number, Boolean, Timestamp, JSON, Vector, Secret:
		default:
			return fmt.Errorf("field %q: unknown type %q (valid: string, text, number, boolean, timestamp, json, vector, secret)", f.Name, f.Type)
		}
		if f.Type == Secret {
			if err := secretRefusal(f); err != nil {
				return err
			}
		}
		if f.Fulltext && f.Type != String && f.Type != Text {
			return fmt.Errorf("field %q: fulltext is only allowed on string or text fields", f.Name)
		}
		if f.Fulltext && f.Name == "rank" {
			return fmt.Errorf("field %q: rank cannot be a fulltext field (reserved by the FTS5 index)", f.Name)
		}
		if f.Enum != nil {
			if f.Type != String {
				return fmt.Errorf("field %q: enum is only allowed on string fields", f.Name)
			}
			if err := ValidateEnum(f.Name, f.Enum); err != nil {
				return err
			}
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
		if f.Default != nil {
			if f.Required {
				return fmt.Errorf("field %q: default is not allowed on required fields (required rejects inserts that omit the field; default fills it — choose one)", f.Name)
			}
			if f.Vectorize {
				return fmt.Errorf("field %q: default is not allowed on vectorize fields (the server embeds caller-supplied text; a defaulted value would not be embedded)", f.Name)
			}
			if IsNowDefault(f.Default) && f.Type != Timestamp {
				if pre, carried := legacy[f.Name]; !carried || !IsNowDefault(pre.Default) {
					return fmt.Errorf("field %q: default %q is only allowed on timestamp fields (the server stamps its current time on each write that omits the field)", f.Name, NowDefault)
				}
			}
		}
	}
	return nil
}

func SecretRefusal(field, option string) error {
	switch option {
	case "fulltext":
		return fmt.Errorf("field %q: fulltext is not allowed on secret fields, because the full-text index would hold the plaintext; keep searchable text in a separate string or text field", field)
	case "vectorize":
		return fmt.Errorf("field %q: vectorize is not allowed on secret fields, because the embedding is computed from the plaintext and would leak it; keep text to embed in a separate string or text field", field)
	case "enum":
		return fmt.Errorf("field %q: enum is not allowed on secret fields, because the list of allowed values would disclose what is stored; use a string field for enumerated values", field)
	case "default":
		return fmt.Errorf("field %q: default is not allowed on secret fields, because the schema stores a default in plaintext; pass the value on each write instead", field)
	}
	return fmt.Errorf("field %q: %s is not allowed on secret fields", field, option)
}

func secretRefusal(f Field) error {
	switch {
	case f.Fulltext:
		return SecretRefusal(f.Name, "fulltext")
	case f.Vectorize:
		return SecretRefusal(f.Name, "vectorize")
	case f.Enum != nil:
		return SecretRefusal(f.Name, "enum")
	case f.Default != nil:
		return SecretRefusal(f.Name, "default")
	}
	return nil
}

func (t TableSchema) SecretFields() []Field {
	var out []Field
	for _, f := range t.Fields {
		if f.Type == Secret {
			out = append(out, f)
		}
	}
	return out
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
	case Vector, Secret:
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

type Inference struct {
	Fields     []Field                  `json:"fields"`
	Warnings   []string                 `json:"warnings"`
	Provenance map[string][]string      `json:"provenance"`
	Evidence   map[string]FieldEvidence `json:"evidence"`
}

type FieldEvidence struct {
	Present int      `json:"present"`
	Nulls   int      `json:"nulls"`
	Types   []string `json:"types"`
}

type inferredKey struct {
	kinds   map[string]bool
	present int
	nulls   int
}

type inferUnit struct {
	base    string
	desired string
	raws    []string
	split   bool
	natural bool
	field   Field
}

func InferSchema(samples []map[string]any) Inference {
	keys := map[string]*inferredKey{}
	for _, s := range samples {
		for k, v := range s {
			st := keys[k]
			if st == nil {
				st = &inferredKey{kinds: map[string]bool{}}
				keys[k] = st
			}
			st.present++
			if isNilValue(v) {
				st.nulls++
				continue
			}
			st.kinds[jsonKind(goKind(v))] = true
		}
	}

	groups := map[string][]string{}
	for raw := range keys {
		base := cleanName(raw)
		groups[base] = append(groups[base], raw)
	}

	var units []*inferUnit
	for base, raws := range groups {
		sort.Strings(raws)
		if len(raws) > 1 && carriedTogether(samples, raws) {
			for _, raw := range raws {
				units = append(units, &inferUnit{base: base, raws: []string{raw}, split: true, natural: raw == base})
			}
			continue
		}
		units = append(units, &inferUnit{base: base, raws: raws, natural: len(raws) == 1 && raws[0] == base})
	}

	for _, u := range units {
		u.field = inferField(samples, keys, u.raws)
		u.desired = u.base
		if u.field.Fulltext && u.base == "rank" {
			u.desired = "rank_"
			u.natural = false
		}
	}
	sort.Slice(units, func(i, j int) bool {
		a, b := units[i], units[j]
		if a.desired != b.desired {
			return a.desired < b.desired
		}
		if a.natural != b.natural {
			return a.natural
		}
		return a.raws[0] < b.raws[0]
	})

	names := make([]string, len(units))
	taken := map[string]bool{}
	for i, u := range units {
		if !taken[u.desired] {
			taken[u.desired] = true
			names[i] = u.desired
		}
	}
	for i, u := range units {
		if names[i] == "" {
			names[i] = freeName(u.desired, taken)
			taken[names[i]] = true
		}
	}

	result := Inference{Provenance: map[string][]string{}, Evidence: map[string]FieldEvidence{}}
	splitNamed := map[string][]string{}
	for i, u := range units {
		if u.split {
			splitNamed[u.base] = append(splitNamed[u.base], fmt.Sprintf("%q as %q", u.raws[0], names[i]))
		}
	}
	reported := map[string]bool{}
	for i, u := range units {
		name := names[i]
		f := u.field
		f.Name = name
		result.Fields = append(result.Fields, f)
		result.Provenance[name] = u.raws
		result.Evidence[name] = evidenceFor(keys, u.raws)

		switch {
		case u.split:
			if !reported[u.base] {
				reported[u.base] = true
				raws := append([]string(nil), groups[u.base]...)
				sort.Strings(raws)
				named := splitNamed[u.base]
				sort.Strings(named)
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("keys %s all collapse to %q but appear together in a sample, so each keeps a field of its own: %s", quotedList(raws), u.base, strings.Join(named, ", ")))
			}
		case len(u.raws) > 1:
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("keys %s collapse to %q and no sample carries two of them, so they were merged into field %q", quotedList(u.raws), u.base, name))
		case u.raws[0] != name:
			raw := u.raws[0]
			switch {
			case u.desired == "rank_" && strings.ToLower(raw) == "rank":
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("key %q was renamed to %q: a full-text field cannot be named rank (reserved by the FTS5 index)", raw, name))
			case reserved[strings.ToLower(raw)]:
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("reserved key %q was renamed to %q", raw, name))
			case sqlKeywords[strings.ToLower(raw)]:
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("SQL keyword key %q was renamed to %q", raw, name))
			default:
				result.Warnings = append(result.Warnings,
					fmt.Sprintf("key %q was sanitized to %q", raw, name))
			}
		}
	}
	sort.SliceStable(result.Fields, func(i, j int) bool { return result.Fields[i].Name < result.Fields[j].Name })
	return result
}

func inferField(samples []map[string]any, keys map[string]*inferredKey, raws []string) Field {
	member := map[string]bool{}
	kinds := map[string]bool{}
	for _, raw := range raws {
		member[raw] = true
		for k := range keys[raw].kinds {
			kinds[k] = true
		}
	}
	matches := func(k string) bool { return member[k] }
	var f Field
	switch {
	case len(kinds) > 1:
		f.Type = JSON
	case kinds["boolean"]:
		f.Type = Boolean
	case kinds["number"]:
		f.Type = Number
	case kinds["string"]:
		f.Type = String
		if allStringsMatch(samples, matches, LooksLikeTimestamp) {
			f.Type = Timestamp
		} else if allStringsMatch(samples, matches, func(s string) bool {
			return len(s) > 200 || strings.ContainsAny(s, "\n")
		}) {
			f.Type = Text
			f.Fulltext = true
		}
	default:
		f.Type = JSON
	}
	return f
}

func carriedTogether(samples []map[string]any, raws []string) bool {
	for _, s := range samples {
		carried := 0
		for _, raw := range raws {
			if _, ok := s[raw]; ok {
				carried++
			}
		}
		if carried > 1 {
			return true
		}
	}
	return false
}

func freeName(desired string, taken map[string]bool) string {
	for n := 2; ; n++ {
		suffix := fmt.Sprintf("_%d", n)
		stem := desired
		if len(stem)+len(suffix) > 64 {
			stem = stem[:64-len(suffix)]
		}
		if candidate := stem + suffix; !taken[candidate] && ValidIdent(candidate) {
			return candidate
		}
	}
}

func evidenceFor(keys map[string]*inferredKey, raws []string) FieldEvidence {
	ev := FieldEvidence{Types: []string{}}
	kinds := map[string]bool{}
	for _, raw := range raws {
		st := keys[raw]
		ev.Present += st.present
		ev.Nulls += st.nulls
		for k := range st.kinds {
			kinds[k] = true
		}
	}
	for k := range kinds {
		ev.Types = append(ev.Types, k)
	}
	sort.Strings(ev.Types)
	return ev
}

func jsonKind(kind string) string {
	if kind == "bool" {
		return "boolean"
	}
	return kind
}

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
