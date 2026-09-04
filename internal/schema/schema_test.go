package schema

import (
	"strings"
	"testing"
	"time"
)

func TestInferFields(t *testing.T) {
	long := "A very detailed finding body." + strings.Repeat(" x", 150)
	fields := InferFields([]map[string]any{
		{"title": "bug", "score": 3.5, "ok": true, "at_time": "2026-09-01T10:00:00Z", "detail": long, "tags": []any{"a"}},
		{"title": "task", "score": 1, "ok": false, "at_time": "2026-09-02", "detail": long},
	})
	byName := map[string]Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	if byName["score"].Type != Number {
		t.Errorf("score: got %s", byName["score"].Type)
	}
	if byName["ok"].Type != Boolean {
		t.Errorf("ok: got %s", byName["ok"].Type)
	}
	if byName["at_time"].Type != Timestamp {
		t.Errorf("at_time: got %s", byName["at_time"].Type)
	}
	if byName["tags"].Type != JSON {
		t.Errorf("tags: got %s", byName["tags"].Type)
	}
	if byName["detail"].Type != Text || !byName["detail"].Fulltext {
		t.Errorf("detail: got %+v", byName["detail"])
	}
	if byName["title"].Type != String {
		t.Errorf("title: got %s", byName["title"].Type)
	}
}

func TestVectorCodecRoundTrip(t *testing.T) {
	in := []float32{1.5, -2.25, 0, 3.75}
	out, err := DecodeVector(EncodeVector(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("length mismatch: %d", len(out))
	}
	for i := range in {
		if in[i] != out[i] {
			t.Fatalf("value %d: %f != %f", i, in[i], out[i])
		}
	}
	if _, err := DecodeVector([]byte{1, 2, 3}); err == nil {
		t.Fatal("expected odd-length blob to fail")
	}
}

func TestValidateRejects(t *testing.T) {
	cases := [][]Field{
		{},
		{{Name: "id", Type: String}},
		{{Name: "Bad", Type: String}},
		{{Name: "a", Type: "bogus"}},
		{{Name: "a", Type: String}, {Name: "a", Type: String}},
		{{Name: "a", Type: Number, Fulltext: true}},
		{{Name: "a", Type: Number, Vectorize: true}},
		{{Name: "a", Type: String, Vectorize: true}, {Name: "b", Type: String, Vectorize: true}},
		{{Name: "a", Type: Vector}},
		{{Name: "a", Type: Vector, Dim: 9999}},
		{{Name: "a", Type: Text, Enum: []string{"x"}}},
		{{Name: "a", Type: Number, Enum: []string{"x"}}},
		{{Name: "a", Type: String, Enum: []string{}}},
		{{Name: "a", Type: String, Enum: []string{"x", "x"}}},
		{{Name: "a", Type: String, Enum: []string{""}}},
	}
	for i, fields := range cases {
		if err := Validate(fields); err == nil {
			t.Errorf("case %d: expected rejection for %+v", i, fields)
		}
	}
}

func TestValidateAccepts(t *testing.T) {
	fields := []Field{
		{Name: "title", Type: String, Fulltext: true},
		{Name: "body", Type: Text, Vectorize: true},
		{Name: "emb", Type: Vector, Dim: 1536},
		{Name: "severity", Type: String, Enum: []string{"SEV0", "SEV1"}, Fulltext: true},
	}
	if err := Validate(fields); err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

func TestEnumAllowsExactMatch(t *testing.T) {
	f := Field{Name: "severity", Type: String, Enum: []string{"SEV0", "SEV1"}}
	if !EnumAllows(f.Enum, "SEV0") || !EnumAllows(f.Enum, "SEV1") {
		t.Fatal("members must be allowed")
	}
	// Exact match only: case variants, trimmed variants, and prefixes are
	// rejected — values are compared as written.
	for _, v := range []string{"sev0", "SEV0 ", "SEV", ""} {
		if EnumAllows(f.Enum, v) {
			t.Fatalf("%q must not be allowed", v)
		}
	}
	// No enum imposes no constraint.
	if !EnumAllows(nil, "anything") {
		t.Fatal("an absent enum must allow every value")
	}
}

func TestInferMixedScalarKindsFallbackToJSON(t *testing.T) {
	fields := InferFields([]map[string]any{
		{"v": true, "n": 1},
		{"v": "unknown", "n": "2"},
	})
	byName := map[string]Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	if byName["v"].Type != JSON {
		t.Errorf("v: got %s, want json", byName["v"].Type)
	}
	if byName["n"].Type != JSON {
		t.Errorf("n: got %s, want json", byName["n"].Type)
	}
}

func TestInferStructuredMixedKindsFallbackToJSON(t *testing.T) {
	fields := InferFields([]map[string]any{
		{"v": true},
		{"v": map[string]any{"state": "unknown"}},
	})
	if fields[0].Type != JSON {
		t.Fatalf("scalar+object mix: got %s, want json", fields[0].Type)
	}
}

func TestInferCaseVariantKeysAreReported(t *testing.T) {
	r := InferSchema([]map[string]any{
		{"Name": "a"},
		{"name": "b"},
	})
	if len(r.Fields) != 1 || r.Fields[0].Name != "name" {
		t.Fatalf("case variants should merge into one field: %+v", r.Fields)
	}
	if r.Fields[0].Type != String {
		t.Fatalf("merged same-kind variants should keep the kind, got %s", r.Fields[0].Type)
	}
	found := false
	for _, w := range r.Warnings {
		if strings.Contains(w, "Name") && strings.Contains(w, "name") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a warning about case collision, got %v", r.Warnings)
	}
	if len(r.Provenance["name"]) != 2 {
		t.Fatalf("expected provenance for name to list both raw keys, got %v", r.Provenance)
	}
}

func TestInferUppercaseKeyStringNotTimestamp(t *testing.T) {
	fields := InferFields([]map[string]any{
		{"Name": "Alice"},
	})
	if len(fields) != 1 || fields[0].Name != "name" || fields[0].Type != String {
		t.Fatalf("plain name should infer string, got %+v", fields)
	}
}

func TestInferSkipsNulls(t *testing.T) {
	fields := InferFields([]map[string]any{
		{"name": "Alice"},
		{"name": nil},
	})
	if len(fields) != 1 || fields[0].Name != "name" || fields[0].Type != String {
		t.Fatalf("nullable string field should infer string, got %+v", fields)
	}
}

func TestInferAllNullKeyRetained(t *testing.T) {
	fields := InferFields([]map[string]any{
		{"name": nil, "x": 1},
	})
	byName := map[string]Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	if _, ok := byName["name"]; !ok {
		t.Fatalf("all-null key must be retained, got %+v", fields)
	}
	if byName["name"].Type != JSON {
		t.Fatalf("all-null key should infer nullable json, got %s", byName["name"].Type)
	}
}

func TestInferInvalidTimestampsStayString(t *testing.T) {
	for _, bad := range []string{"2026-99-99", "2026-01-01T++++", "2026-02-30"} {
		fields := InferFields([]map[string]any{{"at_time": bad}})
		if len(fields) != 1 || fields[0].Type != String {
			t.Fatalf("date-shaped but invalid %q must infer string, got %+v", bad, fields)
		}
	}
}

func TestInferValidTimestampVariants(t *testing.T) {
	for _, good := range []string{
		"2026-09-01",
		"2026-09-01T10:00:00Z",
		"2026-09-01T10:00:00.5+02:00",
		"2026-09-01 10:00:00",
		"2026-09-01T10:00:05",
	} {
		fields := InferFields([]map[string]any{{"at_time": good}})
		if len(fields) != 1 || fields[0].Type != Timestamp {
			t.Fatalf("valid timestamp %q must infer timestamp, got %+v", good, fields)
		}
	}
}

func TestInferOutOfRangeTimestampOffsetsStayString(t *testing.T) {
	for _, bad := range []string{"2026-01-01T00:00:00+24:00", "2026-01-01T00:00:00+23:60", "2026-01-01T00:00:00-25:00"} {
		fields := InferFields([]map[string]any{{"at_time": bad}})
		if len(fields) != 1 || fields[0].Type != String {
			t.Fatalf("out-of-range offset %q must infer string, got %+v", bad, fields)
		}
	}
	for _, good := range []string{"2026-01-01T00:00:00+23:59", "2026-01-01T00:00:00-05:30", "2026-01-01T00:00:00Z"} {
		fields := InferFields([]map[string]any{{"at_time": good}})
		if len(fields) != 1 || fields[0].Type != Timestamp {
			t.Fatalf("in-range offset %q must infer timestamp, got %+v", good, fields)
		}
	}
}

func TestInferGoNumericTypesAreNumbers(t *testing.T) {
	fields := InferFields([]map[string]any{
		{"n": float32(1.5), "m": int32(7), "u": uint64(9)},
	})
	byName := map[string]Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	for _, name := range []string{"n", "m", "u"} {
		if byName[name].Type != Number {
			t.Fatalf("go numeric %q should infer number, got %s", name, byName[name].Type)
		}
	}
}

func TestValidateRejectsDimOnNonVectorField(t *testing.T) {
	err := Validate([]Field{{Name: "s", Type: String, Dim: 4}})
	if err == nil {
		t.Fatal("expected dim on a non-vector field to be rejected")
	}
}

func TestInferLowercaseRFC3339Separators(t *testing.T) {
	for _, good := range []string{"2026-09-01t10:00:00z", "2026-09-01T10:00:00z"} {
		fields := InferFields([]map[string]any{{"at_time": good}})
		if len(fields) != 1 || fields[0].Type != Timestamp {
			t.Fatalf("lowercase rfc3339 %q must infer timestamp, got %+v", good, fields)
		}
	}
}

func TestSQLiteReservedTableNamesRejected(t *testing.T) {
	for _, name := range []string{"sqlite_master", "sqlite_sequence", "sqlite_foo"} {
		if ValidTableName(name) {
			t.Fatalf("sqlite_-prefixed name %q must be rejected", name)
		}
	}
	if !ValidTableName("sqlitey_table") {
		t.Fatal("names that merely start with the letters sqlite must stay valid")
	}
}

func TestSQLKeywordsRejected(t *testing.T) {
	for _, name := range []string{"order", "group", "where", "select", "index", "from", "by", "current_date", "notnull"} {
		if ValidIdent(name) {
			t.Fatalf("SQL keyword %q must be rejected as an identifier", name)
		}
		if ValidTableName(name) {
			t.Fatalf("SQL keyword %q must be rejected as a table name", name)
		}
	}
	// Suffixes and prefixes that are not exact keywords should still be valid.
	for _, name := range []string{"my_order", "order_field", "grouped", "selected", "my_indexed", "orderby"} {
		if !ValidIdent(name) {
			t.Fatalf("non-keyword %q must be accepted", name)
		}
	}
}

func TestValidateRejectsSQLKeywordWithSuggestion(t *testing.T) {
	err := Validate([]Field{{Name: "order", Type: String}})
	if err == nil {
		t.Fatal("expected SQL keyword field name to be rejected")
	}
	if !strings.Contains(err.Error(), "order") || !strings.Contains(err.Error(), "SQLite/SQL keyword") {
		t.Fatalf("expected a clear SQL keyword error, got %v", err)
	}
	if !strings.Contains(err.Error(), "my_order") {
		t.Fatalf("expected error to suggest an alternative, got %v", err)
	}

	err = Validate([]Field{{Name: "id", Type: String}})
	if err == nil {
		t.Fatal("expected reserved internal name to be rejected")
	}
	if !strings.Contains(err.Error(), "record_id") {
		t.Fatalf("expected suggestion for 'id', got %v", err)
	}
}

func TestSuggestIdent(t *testing.T) {
	cases := []struct{ in, want string }{
		{"order", "my_order"},
		{"id", "record_id"},
		{"created_at", "created_time"},
		{"rowid", "record_id"},
		{"select", "my_select"},
		{"by", "my_by"},
		{"title", "title"},
	}
	for _, c := range cases {
		if got := SuggestIdent(c.in); got != c.want {
			t.Fatalf("SuggestIdent(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestValidateForMigrationGrandfathersLegacyFields(t *testing.T) {
	old := []Field{{Name: "order", Type: String}, {Name: "body", Type: Text}}
	// Adding an unrelated field must succeed even though the table has a keyword field.
	if err := ValidateForMigration(append(old, Field{Name: "priority", Type: Number}), old); err != nil {
		t.Fatalf("expected migration to allow legacy keyword field, got %v", err)
	}
	// Renaming a legacy field to a non-keyword name must succeed.
	renamed := []Field{{Name: "my_order", Type: String}, {Name: "body", Type: Text}}
	if err := ValidateForMigration(renamed, old); err != nil {
		t.Fatalf("expected rename away from keyword to succeed, got %v", err)
	}
	// Renaming an unrelated field to a keyword must still fail.
	bad := []Field{{Name: "order", Type: String}}
	if err := ValidateForMigration(bad, nil); err == nil {
		t.Fatal("expected new keyword field to be rejected")
	}
}

func TestIdentPattern(t *testing.T) {
	pat := IdentPattern()
	if !strings.HasPrefix(pat, "^(?!") {
		t.Fatalf("IdentPattern must use a negative lookahead, got %q", pat)
	}
	if !strings.Contains(pat, "[a-z][a-z0-9_]{0,63}$") {
		t.Fatalf("IdentPattern must end with the base identifier pattern, got %q", pat)
	}
	for _, kw := range []string{"order", "select", "where", "group", "index", "id", "rowid"} {
		if !strings.Contains(pat, kw) {
			t.Fatalf("IdentPattern must include %q, got %q", kw, pat)
		}
	}
}

func TestInferDefinedNumericTypesAreNumbers(t *testing.T) {
	type score int64
	type ratio float32
	fields := InferFields([]map[string]any{{"s": score(3), "r": ratio(0.5)}})
	byName := map[string]Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	for _, name := range []string{"s", "r"} {
		if byName[name].Type != Number {
			t.Fatalf("defined numeric %q should infer number, got %s", name, byName[name].Type)
		}
	}
}

func TestInferDefinedStringAndBoolTypes(t *testing.T) {
	type label string
	type flag bool
	fields := InferFields([]map[string]any{{"l": label("hello"), "f": flag(true)}})
	byName := map[string]Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	if byName["l"].Type != String {
		t.Fatalf("defined string type should infer string, got %s", byName["l"].Type)
	}
	if byName["f"].Type != Boolean {
		t.Fatalf("defined bool type should infer boolean, got %s", byName["f"].Type)
	}
}

func TestInferNamedStringTimestampDetection(t *testing.T) {
	type when string
	fields := InferFields([]map[string]any{{"w": when("2026-09-01T10:00:00Z")}})
	if len(fields) != 1 || fields[0].Type != Timestamp {
		t.Fatalf("named string timestamp should infer timestamp, got %+v", fields)
	}
}

func TestInferUintptrSamplesAreNumbers(t *testing.T) {
	type handle uintptr
	fields := InferFields([]map[string]any{{"p": uintptr(4), "h": handle(8)}})
	byName := map[string]Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	for _, name := range []string{"p", "h"} {
		if byName[name].Type != Number {
			t.Fatalf("uintptr sample %q should infer number, got %s", name, byName[name].Type)
		}
	}
}

func TestInferTypedNilValuesSkipped(t *testing.T) {
	var missing *string
	fields := InferFields([]map[string]any{
		{"name": "Alice"},
		{"name": missing},
	})
	if len(fields) != 1 || fields[0].Type != String {
		t.Fatalf("typed nil must not turn a string field into json, got %+v", fields)
	}
}

func TestInferPointerSamplesDereference(t *testing.T) {
	name := "hello"
	when := "2026-09-01T10:00:00Z"
	fields := InferFields([]map[string]any{
		{"s": &name, "w": &when},
		{"s": "plain", "w": "2026-09-02T10:00:00Z"},
	})
	byName := map[string]Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	if byName["s"].Type != String {
		t.Fatalf("pointer string sample should infer string, got %s", byName["s"].Type)
	}
	if byName["w"].Type != Timestamp {
		t.Fatalf("pointer timestamp sample should infer timestamp, got %s", byName["w"].Type)
	}
}

func TestInferTypedNilTimestampStaysTimestamp(t *testing.T) {
	var missing *string
	fields := InferFields([]map[string]any{
		{"at_time": "2026-09-01T10:00:00Z"},
		{"at_time": missing},
	})
	if len(fields) != 1 || fields[0].Type != Timestamp {
		t.Fatalf("typed nil must not downgrade a timestamp field, got %+v", fields)
	}
}

func TestInferPointerChainToNilSkipped(t *testing.T) {
	var p *string
	fields := InferFields([]map[string]any{
		{"name": &p},
		{"name": "Alice"},
	})
	if len(fields) != 1 || fields[0].Type != String {
		t.Fatalf("pointer chain ending in nil must be skipped, got %+v", fields)
	}
}

func TestInferBoxedInterfaceChains(t *testing.T) {
	var p *string
	boxedNil := any(p)
	str := "2026-09-01T10:00:00Z"
	boxedStr := any(str)
	fields := InferFields([]map[string]any{
		{"a": &boxedNil, "b": &boxedStr},
		{"a": "x", "b": "2026-09-02T10:00:00Z"},
	})
	byName := map[string]Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	if byName["a"].Type != String {
		t.Fatalf("boxed typed nil behind a pointer must be skipped, got %s", byName["a"].Type)
	}
	if byName["b"].Type != Timestamp {
		t.Fatalf("boxed string behind a pointer must feed subtype inference, got %s", byName["b"].Type)
	}
}

func TestInferTimeTimeSamplesAreTimestamps(t *testing.T) {
	ts := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	fields := InferFields([]map[string]any{{"t": ts}, {"t": &ts}})
	if len(fields) != 1 || fields[0].Type != Timestamp {
		t.Fatalf("time.Time samples should infer timestamp, got %+v", fields)
	}
}

func TestInferSelfReferentialSampleTerminates(t *testing.T) {
	var v any
	v = &v
	done := make(chan []Field, 1)
	go func() { done <- InferFields([]map[string]any{{"x": v}, {"x": "hello"}}) }()
	select {
	case fields := <-done:
		if len(fields) != 1 || fields[0].Type != String {
			t.Fatalf("cyclic value is skipped like nil, so the string sample should infer string, got %+v", fields)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("inference did not terminate on a self-referential sample")
	}
}

func TestInferDeepPointerChainClassifies(t *testing.T) {
	var v any = "hello"
	for i := 0; i < 100; i++ {
		boxed := v
		v = &boxed
	}
	fields := InferFields([]map[string]any{{"x": v}})
	if len(fields) != 1 || fields[0].Type != String {
		t.Fatalf("deep acyclic pointer chain should still classify as string, got %+v", fields)
	}
}

func TestInferFieldsEmitsValidIdents(t *testing.T) {
	fields := InferFields([]map[string]any{
		{"1st": "one", "my-field": "two", "ID": "three", "created_at": "four", "ok field": 5},
	})
	for _, f := range fields {
		if !ValidIdent(f.Name) {
			t.Fatalf("inferred name %q is not a valid identifier", f.Name)
		}
	}
	names := map[string]bool{}
	for _, f := range fields {
		names[f.Name] = true
	}
	for _, want := range []string{"x1st", "my_field", "id_", "created_at_", "ok_field"} {
		if !names[want] {
			t.Fatalf("expected sanitized name %q, got %+v", want, fields)
		}
	}
}

func TestInferSchemaRoundTripsThroughValidate(t *testing.T) {
	samples := []map[string]any{
		{"1st": "one", "my-field": "two", "Name": "Alice", "name": "Bob", "id": 1},
	}
	fields := InferFields(samples)
	if err := Validate(fields); err != nil {
		t.Fatalf("inferred fields must pass Validate: %v", err)
	}
}

func TestInferSchemaSanitizesSQLKeywordKeys(t *testing.T) {
	// Common sample keys that are SQLite/SQL keywords must infer usable names
	// (the documented infer-then-create workflow must not dead-end at
	// create_table's keyword rejection).
	r := InferSchema([]map[string]any{
		{"order": "first", "group": "g", "index": 3, "plan": "free", "select": "x"},
	})
	byName := map[string]Field{}
	for _, f := range r.Fields {
		byName[f.Name] = f
	}
	for _, want := range []string{"order_", "group_", "index_", "plan_", "select_"} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("expected keyword key to be renamed to %q, got %+v", want, r.Fields)
		}
	}
	if err := Validate(r.Fields); err != nil {
		t.Fatalf("inferred fields must pass Validate: %v", err)
	}
	found := false
	for _, w := range r.Warnings {
		if strings.Contains(w, "SQL keyword key") && strings.Contains(w, `"order"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a SQL keyword rename warning, got %v", r.Warnings)
	}
	if raws := r.Provenance["order_"]; len(raws) != 1 || raws[0] != "order" {
		t.Fatalf("provenance must map order_ back to the raw key, got %v", r.Provenance["order_"])
	}
}

func TestInferSchemaReportsSanitizationWarnings(t *testing.T) {
	r := InferSchema([]map[string]any{
		{"1st": "one", "my-field": "two", "id": "three", "created_at": "four"},
	})
	if len(r.Warnings) == 0 {
		t.Fatalf("expected warnings for sanitized/keys, got none")
	}
	if len(r.Provenance) == 0 {
		t.Fatalf("expected provenance")
	}
	for _, f := range r.Fields {
		if !ValidIdent(f.Name) {
			t.Fatalf("inferred name %q is not a valid identifier", f.Name)
		}
	}
}
