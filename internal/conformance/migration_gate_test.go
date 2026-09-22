package conformance

import (
	"net/http"
	"strings"
	"testing"
)

func seedGatedTable(t *testing.T) *harness {
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
	h.mustHTTP("insert", map[string]any{
		"namespace": "acme", "table": "notes",
		"records": []map[string]any{
			{"body": "the merger closes friday", "kind": "confidential", "scratch": "x"},
			{"body": "second hidden note", "kind": "internal", "scratch": "y"},
		},
	})
	return h
}

var dataDependentMigrations = []struct {
	name    string
	changes string
}{
	{"set_enum", `[{"op":"set_enum","name":"kind","values":["public"]}]`},
	{"set_vectorize enabling", `[{"op":"set_vectorize","name":"body","value":true}]`},
	{"set_fulltext enabling", `[{"op":"set_fulltext","name":"body","value":true}]`},
	{"add_field with a backfill default", `[{"op":"add_field","field":{"name":"tag","type":"string"},"default":"none"}]`},
	{"add_field of a required field", `[{"op":"add_field","field":{"name":"owner_ref","type":"string","required":true},"default":"none"}]`},
	{"drop_field", `[{"op":"drop_field","name":"scratch"}]`},
	{"set_row_access enabling", `[{"op":"set_row_access","value":true}]`},
}

func migrateAs(t *testing.T, h *harness, who, changes string) (int, map[string]any) {
	t.Helper()
	res, out := h.asIdentity(t, who, "", "migrate",
		`{"namespace":"acme","table":"notes","changes":`+changes+`}`)
	return res.StatusCode, out
}

func TestEveryDataDependentMigrationNeedsTableWideRead(t *testing.T) {
	for _, tc := range dataDependentMigrations {
		t.Run(tc.name, func(t *testing.T) {
			h := seedGatedTable(t)
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
			if status, out := migrateAs(t, h, "dave", tc.changes); status == http.StatusForbidden {
				t.Fatalf("the same migration is still refused once table-wide read is granted, so the 403 above was not this gate: %v", out)
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

var specUngatedMigrations = []struct {
	name    string
	changes string
}{
	{"add_field of a nullable field with no backfill", `[{"op":"add_field","field":{"name":"tag","type":"string"}}]`},
	{"add_field of a vector field with no backfill", `[{"op":"add_field","field":{"name":"v","type":"vector","dim":4}}]`},
}

func TestTheGateIsStricterThanTheSpecForABackfillFreeAddField(t *testing.T) {
	for _, tc := range specUngatedMigrations {
		t.Run(tc.name, func(t *testing.T) {
			h := seedGatedTable(t)
			grantTo(t, h, "principal", "dave", "acme", "notes", "schema")
			status, out := migrateAs(t, h, "dave", tc.changes)
			if status != http.StatusForbidden {
				t.Fatalf("this migration is no longer refused to a schema-only caller (status %d), so the gate now matches identity-and-engines.md line 547: delete this test and fold the case into TestEveryDataDependentMigrationNeedsTableWideRead's counterpart", status)
			}
			body := mustJSON(t, out)
			if !strings.Contains(body, "depends on the table's existing rows") {
				t.Fatalf("refused for some other reason than the data-dependent gate, so this test is not recording what it claims: %s", body)
			}
		})
	}
}
