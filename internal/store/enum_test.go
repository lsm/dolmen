package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func enumPtr(vals ...string) *[]string {
	v := make([]string, len(vals))
	copy(v, vals)
	return &v
}

func mustCreateSeverity(t *testing.T, st legacyStore) {
	t.Helper()
	mustNS(t, st, "test")
	if _, err := st.CreateTable(context.Background(), "test", "incidents", []schema.Field{
		{Name: "title", Type: schema.String},
		{Name: "severity", Type: schema.String, Enum: []string{"SEV0", "SEV1", "SEV2", "SEV3"}},
	}); err != nil {
		t.Fatalf("create table: %v", err)
	}
}

func TestEnumCreateTableValidation(t *testing.T) {
	st := openStore(t)
	mustNS(t, st, "test")
	ctx := context.Background()

	if _, err := st.CreateTable(ctx, "test", "bad_type", []schema.Field{
		{Name: "title", Type: schema.Text, Enum: []string{"a"}},
	}); err == nil || !strings.Contains(err.Error(), `field "title": enum is only allowed on string fields`) {
		t.Fatalf("enum on text field must be rejected with the field named, got %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "bad_empty", []schema.Field{
		{Name: "title", Type: schema.String, Enum: []string{}},
	}); err == nil || !strings.Contains(err.Error(), `field "title": enum must list at least one value`) {
		t.Fatalf("empty enum must be rejected, got %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "bad_dup", []schema.Field{
		{Name: "title", Type: schema.String, Enum: []string{"a", "a"}},
	}); err == nil || !strings.Contains(err.Error(), `field "title": enum lists duplicate value "a"`) {
		t.Fatalf("duplicate enum values must be rejected, got %v", err)
	}

	if _, err := st.CreateTable(ctx, "test", "bad_empty_val", []schema.Field{
		{Name: "title", Type: schema.String, Enum: []string{""}},
	}); err == nil || !strings.Contains(err.Error(), `field "title": enum values must not be empty strings`) {
		t.Fatalf("empty enum value must be rejected, got %v", err)
	}

	if _, err := st.CreateTable(ctx, "test", "bad_default", []schema.Field{
		{Name: "severity", Type: schema.String, Enum: []string{"SEV0", "SEV1"}, Default: "SEV9"},
	}); err == nil || !strings.Contains(err.Error(), `field "severity": value "SEV9" is not one of the allowed enum values (SEV0, SEV1)`) {
		t.Fatalf("non-member default must be rejected at create, got %v", err)
	}

	if _, err := st.CreateTable(ctx, "test", "good_default", []schema.Field{
		{Name: "title", Type: schema.String},
		{Name: "severity", Type: schema.String, Enum: []string{"SEV0", "SEV1"}, Default: "SEV1"},
	}); err != nil {
		t.Fatalf("member default must be accepted: %v", err)
	}
	ids, err := st.Insert(ctx, "test", "good_default", []map[string]any{{"title": "x"}}, testEmbed)
	if err != nil {
		t.Fatalf("insert with defaulted enum field: %v", err)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT severity FROM good_default WHERE id = ?", []any{ids[0]}, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if rows[0]["severity"] != "SEV1" {
		t.Fatalf("omitted enum field must store the default, got %v", rows[0]["severity"])
	}
}

func TestEnumInsertRejectsNonMember(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	raw, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	st := legacy(raw)
	mustCreateSeverity(t, st)

	_, err = st.Insert(ctx, "test", "incidents", []map[string]any{
		{"title": "typo", "severity": "opn"},
	}, testEmbed)
	if err == nil {
		t.Fatal("non-member enum value must be rejected")
	}
	msg := err.Error()
	for _, want := range []string{`field "severity"`, `"opn"`, "SEV0, SEV1, SEV2, SEV3"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("rejection must name the field, the rejected value, and the allowed values (want %q in %q)", want, msg)
		}
	}

	if _, err := st.Insert(ctx, "test", "incidents", []map[string]any{
		{"title": "lowercased", "severity": "sev1"},
	}, testEmbed); err == nil || !strings.Contains(err.Error(), `"sev1"`) {
		t.Fatalf("lowercased value must be rejected as written, got %v", err)
	}

	ids, err := st.Insert(ctx, "test", "incidents", []map[string]any{
		{"title": "ok", "severity": "SEV2"},
	}, testEmbed)
	if err != nil {
		t.Fatalf("member value must be accepted: %v", err)
	}
	rows, _, err := st.Query(ctx, "test", "SELECT severity FROM incidents WHERE id = ?", []any{ids[0]}, 0, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if rows[0]["severity"] != "SEV2" {
		t.Fatalf("member value must be stored as written, got %v", rows[0]["severity"])
	}

	if _, err := st.Insert(ctx, "test", "incidents", []map[string]any{
		{"title": "cleared", "severity": nil},
	}, testEmbed); err != nil {
		t.Fatalf("explicit null must pass the enum: %v", err)
	}

	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	raw2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	st2 := legacy(raw2)
	defer st2.Close()
	if _, err := st2.Insert(ctx, "test", "incidents", []map[string]any{
		{"title": "after reopen", "severity": "SEV9"},
	}, testEmbed); err == nil || !strings.Contains(err.Error(), "SEV0, SEV1, SEV2, SEV3") {
		t.Fatalf("enum must survive the schema round-trip, got %v", err)
	}
}

func TestEnumUpdateAndUpsertPaths(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	mustCreateSeverity(t, st)
	ids, err := st.Insert(ctx, "test", "incidents", []map[string]any{
		{"title": "one", "severity": "SEV1"},
	}, testEmbed)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	if _, err := st.Update(ctx, "test", "incidents", "id = ?", []any{ids[0]}, map[string]any{"severity": "urgent"}, testEmbed); err == nil ||
		!strings.Contains(err.Error(), `field "severity": value "urgent" is not one of the allowed enum values (SEV0, SEV1, SEV2, SEV3)`) {
		t.Fatalf("update with non-member must be rejected with the pinned message, got %v", err)
	}
	if _, err := st.Update(ctx, "test", "incidents", "id = ?", []any{ids[0]}, map[string]any{"severity": "SEV0"}, testEmbed); err != nil {
		t.Fatalf("update with member: %v", err)
	}

	if _, err := st.Upsert(ctx, "test", "incidents", "id = ?", []any{ids[0]}, map[string]any{"severity": "zzz"}, testEmbed); err == nil ||
		!strings.Contains(err.Error(), `"zzz"`) {
		t.Fatalf("upsert set with non-member must be rejected, got %v", err)
	}

	if _, err := st.Upsert(ctx, "test", "incidents", "id = ?", []any{999999}, map[string]any{"title": "two", "severity": "nope"}, testEmbed); err == nil ||
		!strings.Contains(err.Error(), `"nope"`) {
		t.Fatalf("upsert insert path must reject non-members, got %v", err)
	}

	if _, _, _, err := st.UpsertByKey(ctx, "test", "incidents", []string{"title"}, []map[string]any{
		{"title": "one", "severity": "bad"},
	}, testEmbed); err == nil || !strings.Contains(err.Error(), `"bad"`) {
		t.Fatalf("upsert_by_key update path must reject non-members, got %v", err)
	}
	if _, _, _, err := st.UpsertByKey(ctx, "test", "incidents", []string{"title"}, []map[string]any{
		{"title": "three", "severity": "bad2"},
	}, testEmbed); err == nil || !strings.Contains(err.Error(), `"bad2"`) {
		t.Fatalf("upsert_by_key insert path must reject non-members, got %v", err)
	}

	if _, _, _, err := st.UpsertByKey(ctx, "test", "incidents", []string{"title"}, []map[string]any{
		{"title": "one", "severity": "SEV3"},
		{"title": "four", "severity": "SEV0"},
	}, testEmbed); err != nil {
		t.Fatalf("upsert_by_key with members: %v", err)
	}
}

func TestEnumSetEnumLifecycle(t *testing.T) {
	st := openStore(t)
	mustNS(t, st, "test")
	ctx := context.Background()

	if _, err := st.CreateTable(ctx, "test", "incidents", []schema.Field{
		{Name: "title", Type: schema.String},
		{Name: "severity", Type: schema.String},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "incidents", []map[string]any{
		{"title": "a", "severity": "SEV1"},
		{"title": "b", "severity": "SEV1"},
		{"title": "c", "severity": "opn"},
	}, testEmbed); err != nil {
		t.Fatalf("insert free strings: %v", err)
	}

	_, err := st.Migrate(ctx, "test", "incidents", []schema.Change{
		{Op: schema.OpSetEnum, Name: "severity", Enum: enumPtr("SEV0", "SEV1")},
	}, testEmbed, 1)
	if err == nil || !strings.Contains(err.Error(), `"opn" is stored by 1 rows`) {
		t.Fatalf("constraining over a non-member value must be rejected with the count, got %v", err)
	}

	if _, err := st.PlanMigration(ctx, "test", "incidents", []schema.Change{
		{Op: schema.OpSetEnum, Name: "severity", Enum: enumPtr("SEV0", "SEV1")},
	}, testEmbed, 1); err == nil || !strings.Contains(err.Error(), `"opn" is stored by 1 rows`) {
		t.Fatalf("dry_run must run the same verification, got %v", err)
	}

	if _, err := st.Update(ctx, "test", "incidents", "title = 'c'", nil, map[string]any{"severity": "SEV2"}, testEmbed); err != nil {
		t.Fatalf("fix typo: %v", err)
	}
	sc, err := st.Migrate(ctx, "test", "incidents", []schema.Change{
		{Op: schema.OpSetEnum, Name: "severity", Enum: enumPtr("SEV0", "SEV1", "SEV2")},
	}, testEmbed, 1)
	if err != nil {
		t.Fatalf("set_enum after cleanup: %v", err)
	}
	if sc.Version != 2 {
		t.Fatalf("set_enum must bump the version, got %d", sc.Version)
	}
	if f := sc.Field("severity"); f == nil || len(f.Enum) != 3 || f.Enum[2] != "SEV2" {
		t.Fatalf("schema must carry the new enum, got %+v", sc.Field("severity"))
	}

	if _, err := st.Insert(ctx, "test", "incidents", []map[string]any{
		{"title": "d", "severity": "SEV3"},
	}, testEmbed); err == nil || !strings.Contains(err.Error(), "(SEV0, SEV1, SEV2)") {
		t.Fatalf("post-narrowing write must be rejected, got %v", err)
	}

	if _, err := st.Migrate(ctx, "test", "incidents", []schema.Change{
		{Op: schema.OpSetEnum, Name: "severity", Enum: enumPtr("SEV0")},
	}, testEmbed, 2); err == nil || !strings.Contains(err.Error(), `"SEV1" is stored by 2 rows`) {
		t.Fatalf("removing an in-use value must be rejected with the count, got %v", err)
	}

	if _, err := st.Migrate(ctx, "test", "incidents", []schema.Change{
		{Op: schema.OpSetEnum, Name: "severity", Enum: enumPtr("SEV0", "SEV1", "SEV2", "SEV3")},
	}, testEmbed, 2); err != nil {
		t.Fatalf("adding a value must be safe: %v", err)
	}

	sc, err = st.Migrate(ctx, "test", "incidents", []schema.Change{
		{Op: schema.OpSetEnum, Name: "severity", Enum: enumPtr()},
	}, testEmbed, 3)
	if err != nil {
		t.Fatalf("clearing the enum: %v", err)
	}
	if f := sc.Field("severity"); f == nil || f.Enum != nil {
		t.Fatalf("cleared field must carry no enum, got %+v", sc.Field("severity"))
	}
	if _, err := st.Insert(ctx, "test", "incidents", []map[string]any{
		{"title": "free again", "severity": "anything"},
	}, testEmbed); err != nil {
		t.Fatalf("writes must be unconstrained after clearing: %v", err)
	}

	ms, err := st.ListMigrations(ctx, "test", "incidents")
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	var last []schema.Change
	for _, m := range ms {
		if m.ToVersion == 4 {
			last = m.Changes
		}
	}
	if len(last) != 1 || last[0].Op != schema.OpSetEnum || last[0].Enum == nil || len(*last[0].Enum) != 0 {
		t.Fatalf("history must record set_enum with its explicit empty array, got %+v", last)
	}
	raw, _ := json.Marshal(last)
	if !strings.Contains(string(raw), `"enum":[]`) {
		t.Fatalf("the clearing change must serialize with an explicit empty array, got %s", raw)
	}
}

func TestEnumSetEnumValidation(t *testing.T) {
	st := openStore(t)
	mustNS(t, st, "test")
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "incidents", []schema.Field{
		{Name: "title", Type: schema.Text},
		{Name: "severity", Type: schema.String, Enum: []string{"SEV0", "SEV1"}, Default: "SEV0"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := st.Migrate(ctx, "test", "incidents", []schema.Change{
		{Op: schema.OpSetEnum, Name: "title", Enum: enumPtr("a")},
	}, testEmbed, 1); err == nil || !strings.Contains(err.Error(), `field "title": enum is only allowed on string fields (this field has type text)`) {
		t.Fatalf("set_enum on text field must be rejected, got %v", err)
	}

	if _, err := st.Migrate(ctx, "test", "incidents", []schema.Change{
		{Op: schema.OpSetEnum, Name: "severity", Enum: enumPtr("SEV0", "SEV0")},
	}, testEmbed, 1); err == nil || !strings.Contains(err.Error(), `duplicate value "SEV0"`) {
		t.Fatalf("duplicate vocabulary must be rejected, got %v", err)
	}
	if _, err := st.Migrate(ctx, "test", "incidents", []schema.Change{
		{Op: schema.OpSetEnum, Name: "severity", Enum: enumPtr("")},
	}, testEmbed, 1); err == nil || !strings.Contains(err.Error(), `enum values must not be empty strings`) {
		t.Fatalf("empty-string vocabulary must be rejected, got %v", err)
	}

	if _, err := st.Migrate(ctx, "test", "incidents", []schema.Change{
		{Op: schema.OpSetEnum, Name: "severity", Enum: enumPtr("SEV1", "SEV2")},
	}, testEmbed, 1); err == nil || !strings.Contains(err.Error(), `the declared default "SEV0" is not in the new enum (SEV1, SEV2)`) {
		t.Fatalf("dropping the default value must be rejected, got %v", err)
	}

	if _, err := st.Migrate(ctx, "test", "incidents", []schema.Change{
		{Op: schema.OpDropField, Name: "severity", Enum: enumPtr("SEV1")},
	}, testEmbed, 1); err == nil || !strings.Contains(err.Error(), "enum is only allowed on set_enum") {
		t.Fatalf("enum on drop_field must be rejected, got %v", err)
	}
	if _, err := st.Migrate(ctx, "test", "incidents", []schema.Change{
		{Op: schema.OpSetEnum, Name: "severity"},
	}, testEmbed, 1); err == nil || !strings.Contains(err.Error(), "set_enum requires an explicit enum array") {
		t.Fatalf("set_enum without an enum array must be rejected, got %v", err)
	}

	if _, err := st.Migrate(ctx, "test", "incidents", []schema.Change{
		{Op: schema.OpSetEnum, Name: "severity", Enum: enumPtr("SEV0", "SEV1", "SEV2")},
	}, testEmbed, 0); err != nil {
		t.Fatalf("set_enum without expected_version: %v", err)
	}
}

func TestEnumAddFieldAndOrthogonality(t *testing.T) {
	st := openStore(t)
	mustNS(t, st, "test")
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "notes", []schema.Field{
		{Name: "body", Type: schema.Text, Fulltext: true},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{{"body": "payment received"}}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}

	if _, err := st.Migrate(ctx, "test", "notes", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "status", Type: schema.String, Enum: []string{"open", "done"}}, Default: "closed"},
	}, testEmbed, 1); err == nil || !strings.Contains(err.Error(), `field "status": value "closed" is not one of the allowed enum values (open, done)`) {
		t.Fatalf("non-member backfill default must be rejected, got %v", err)
	}
	sc, err := st.Migrate(ctx, "test", "notes", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "status", Type: schema.String, Enum: []string{"open", "done"}}, Default: "open"},
	}, testEmbed, 1)
	if err != nil {
		t.Fatalf("add_field with enum and member default: %v", err)
	}
	if f := sc.Field("status"); f == nil || len(f.Enum) != 2 {
		t.Fatalf("added field must carry its enum, got %+v", sc.Field("status"))
	}

	if _, err := st.Update(ctx, "test", "notes", "1=1", nil, map[string]any{"status": "finished"}, testEmbed); err == nil ||
		!strings.Contains(err.Error(), `(open, done)`) {
		t.Fatalf("update on added enum field must be constrained, got %v", err)
	}

	if _, err := st.Migrate(ctx, "test", "notes", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "bucket", Type: schema.String, Enum: []string{"a", "b"}, Fulltext: true}},
	}, testEmbed, 2); err != nil {
		t.Fatalf("enum + fulltext must compose: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"body": "second", "bucket": "c"},
	}, testEmbed); err == nil || !strings.Contains(err.Error(), `value "c" is not one of the allowed enum values (a, b)`) {
		t.Fatalf("enum must hold on a fulltext field, got %v", err)
	}
	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"body": "second", "bucket": "b"},
	}, testEmbed); err != nil {
		t.Fatalf("member on fulltext field: %v", err)
	}
	rows, _, err := st.SearchFulltext(ctx, "test", "notes", "bucket:b", 0, 10, false, "", nil)
	if err != nil {
		t.Fatalf("enum value must be fulltext-searchable: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected the enum-valued row in the index, got %v", rows)
	}
}
