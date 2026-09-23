package conformance

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func seedGatedTable(t *testing.T) *harness {
	t.Helper()
	return seedGatedTableWithRows(t, true)
}

func seedGatedTableWithRows(t *testing.T, withRows bool) *harness {
	t.Helper()
	h := newHarnessMode(t, authGateway)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	h.mustHTTP("create_table", map[string]any{
		"namespace": "acme",
		"table":     "notes",
		"fields": []map[string]any{
			{"name": "body", "type": "text"},
			{"name": "kind", "type": "string"},
			{"name": "scratch", "type": "string"},
		},
	})
	if withRows {
		h.mustHTTP("insert", map[string]any{
			"namespace": "acme", "table": "notes",
			"records": []map[string]any{
				{"body": "the merger closes friday", "kind": "confidential", "scratch": "x"},
				{"body": "second hidden note", "kind": "internal", "scratch": "y"},
			},
		})
	}
	return h
}

var dataDependentMigrations = []struct {
	name    string
	changes string
	noRows  bool
}{
	{name: "set_enum", changes: `[{"op":"set_enum","name":"kind","enum":["confidential","internal"]}]`},
	{name: "set_vectorize enabling", changes: `[{"op":"set_vectorize","name":"body","value":true}]`},
	{name: "set_fulltext enabling", changes: `[{"op":"set_fulltext","name":"body","value":true}]`},
	{name: "add_field with a backfill default", changes: `[{"op":"add_field","field":{"name":"tag","type":"string"},"default":"none"}]`},
	{name: "add_field of a required field", changes: `[{"op":"add_field","field":{"name":"owner_ref","type":"string","required":true},"default":"none"}]`},
	{name: "add_field of a required field with no default", changes: `[{"op":"add_field","field":{"name":"must","type":"string","required":true}}]`, noRows: true},
	{name: "add_field of a full-text field", changes: `[{"op":"add_field","field":{"name":"summary","type":"text","fulltext":true}}]`},
	{name: "add_field of a vectorized field", changes: `[{"op":"add_field","field":{"name":"gist","type":"text","vectorize":true}}]`},
	{name: "drop_field", changes: `[{"op":"drop_field","name":"scratch"}]`},
	{name: "set_row_access enabling", changes: `[{"op":"set_row_access","value":true}]`, noRows: true},
}

func migrateAs(t *testing.T, h *harness, who, changes string) (int, map[string]any) {
	t.Helper()
	res, out := h.asIdentity(t, who, "", "migrate",
		`{"namespace":"acme","table":"notes","changes":`+changes+`}`)
	return res.StatusCode, out
}

func applyWithPrecondition(t *testing.T, h *harness, who, changes string) (int, map[string]any) {
	t.Helper()
	res, out := h.asIdentity(t, who, "", "migrate",
		`{"namespace":"acme","table":"notes","changes":`+changes+`,"expected_version":1,"dry_run":true}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the dry run a destructive apply needs a token from: status %d %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	plan, _ := data["plan"].(map[string]any)
	token, _ := plan["expected_incarnation"].(string)
	if token == "" {
		t.Fatalf("the dry run returned no expected_incarnation: %v", plan)
	}
	res, out = h.asIdentity(t, who, "", "migrate",
		`{"namespace":"acme","table":"notes","changes":`+changes+
			`,"expected_version":1,"expected_incarnation":"`+token+`"}`)
	return res.StatusCode, out
}

func TestEveryDataDependentMigrationNeedsTableWideRead(t *testing.T) {
	for _, tc := range dataDependentMigrations {
		t.Run(tc.name, func(t *testing.T) {
			h := seedGatedTableWithRows(t, !tc.noRows)
			grantTo(t, h, "principal", "dave", "acme", "notes", "schema")

			status, out := migrateAs(t, h, "dave", tc.changes)
			if status != http.StatusForbidden {
				t.Fatalf("a schema-only caller: status %d, want 403: %v", status, out)
			}
			body := mustJSON(t, out)
			for _, secret := range []string{"merger closes friday", "confidential", "internal", "2 rows", "1 rows"} {
				if strings.Contains(body, secret) {
					t.Fatalf("the refusal disclosed %q to a caller who may not read the table: %s", secret, body)
				}
			}

			grantTo(t, h, "principal", "dave", "acme", "notes", "read")
			if status, out := applyWithPrecondition(t, h, "dave", tc.changes); status != http.StatusOK {
				t.Fatalf("the same migration must run once table-wide read is granted, or the 403 above is not evidence of this gate: status %d %v", status, out)
			}
		})
	}
}

func TestADeniedMigrationNeverReachesTheEmbeddingProvider(t *testing.T) {
	h := seedGatedTable(t)
	grantTo(t, h, "principal", "dave", "acme", "notes", "schema")
	before := h.emb.callCount()

	status, out := migrateAs(t, h, "dave", `[{"op":"set_vectorize","name":"body","value":true}]`)
	if status != http.StatusForbidden {
		t.Fatalf("a schema-only caller enabling vectorize: status %d, want 403: %v", status, out)
	}
	if got := h.emb.callCount(); got != before {
		t.Fatalf("the denial landed after %d provider call(s): a schema-only caller must not be able to send rows they cannot read to a third party", got-before)
	}
	for _, text := range h.emb.embeddedTexts() {
		if strings.Contains(text, "merger closes friday") || strings.Contains(text, "second hidden note") {
			t.Fatalf("a hidden row reached the embedding provider at a schema-only caller's direction: %q", text)
		}
	}

	grantTo(t, h, "principal", "dave", "acme", "notes", "read")
	if status, out := migrateAs(t, h, "dave", `[{"op":"set_vectorize","name":"body","value":true}]`); status != http.StatusOK {
		t.Fatalf("with table-wide read the same migration must run: status %d %v", status, out)
	}
	if h.emb.callCount() == before {
		t.Fatal("the permitted migration embedded nothing, so the count above proves nothing about where the denial landed")
	}
}

var dataIndependentMigrations = []struct {
	name    string
	changes string
}{
	{"add_field of a nullable field with no backfill", `[{"op":"add_field","field":{"name":"tag","type":"string"}}]`},
	{"add_field of a vector field with no backfill", `[{"op":"add_field","field":{"name":"v","type":"vector","dim":4}}]`},
	{"add_field of an enum field with no backfill", `[{"op":"add_field","field":{"name":"stage","type":"string","enum":["a","b"]}}]`},
}

func TestADataIndependentMigrationNeedsOnlySchema(t *testing.T) {
	for _, tc := range dataIndependentMigrations {
		t.Run(tc.name, func(t *testing.T) {
			h := seedGatedTable(t)
			grantTo(t, h, "principal", "dave", "acme", "notes", "schema")
			res, out := h.asIdentity(t, "dave", "", "migrate",
				`{"namespace":"acme","table":"notes","changes":`+tc.changes+`,"dry_run":true}`)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("a schema-only caller's dry run: status %d %v; the spec leaves this migration on the schema verb alone, because nothing it plans or applies touches a row", res.StatusCode, out)
			}
			if status, out := migrateAs(t, h, "dave", tc.changes); status != http.StatusOK {
				t.Fatalf("a schema-only caller's apply: status %d %v", status, out)
			}
		})
	}
}

func schemaOnlyDryRun(t *testing.T, withRows bool, changes string) string {
	t.Helper()
	h := seedGatedTableWithRows(t, withRows)
	grantTo(t, h, "principal", "dave", "acme", "notes", "schema")
	res, out := h.asIdentity(t, "dave", "", "migrate",
		`{"namespace":"acme","table":"notes","changes":`+changes+`,"expected_version":1,"dry_run":true}`)
	if errEnv, _ := out["error"].(map[string]any); errEnv != nil {
		delete(errEnv, "request_id")
	}
	if data, _ := out["data"].(map[string]any); data != nil {
		if plan, _ := data["plan"].(map[string]any); plan != nil {
			delete(plan, "expected_incarnation")
		}
	}
	return fmt.Sprintf("%d %s", res.StatusCode, mustJSON(t, out))
}

func TestASchemaOnlyCallerCannotTellAnEmptyTableFromAPopulatedOne(t *testing.T) {
	var corpus []string
	for _, tc := range dataDependentMigrations {
		corpus = append(corpus, tc.changes)
	}
	for _, tc := range dataIndependentMigrations {
		corpus = append(corpus, tc.changes)
	}
	corpus = append(corpus, `[{"op":"rename_field","from":"scratch","to":"notes_scratch"}]`)
	allowed := 0
	for _, changes := range corpus {
		empty, populated := schemaOnlyDryRun(t, false, changes), schemaOnlyDryRun(t, true, changes)
		if empty != populated {
			t.Errorf("%s: a schema-only caller sees\n  %s\non an empty table and\n  %s\non a populated one, so the answer discloses rows they may not read", changes, empty, populated)
		}
		if strings.HasPrefix(empty, "200 ") {
			allowed++
		}
	}
	if allowed == 0 {
		t.Fatal("no migration in the corpus was allowed to a schema-only caller, so every comparison above was between two refusals and proves nothing about the ones the gate lets through")
	}
}
