package blackbox

import (
	"testing"
)

func TestStage02Provision(t *testing.T) {
	op(t, "create_namespace", map[string]any{"namespace": scenarioNamespace})

	tickets := map[string]any{
		"namespace": scenarioNamespace,
		"table":     "tickets",
		"fields": []any{
			map[string]any{"name": "subject", "type": "string", "fulltext": true},
			map[string]any{"name": "body", "type": "text", "fulltext": true},
			map[string]any{"name": "status", "type": "string", "enum": []any{"open", "triaged", "resolved"}},
		},
	}
	kbArticles := map[string]any{
		"namespace": scenarioNamespace,
		"table":     "kb_articles",
		"fields": []any{
			map[string]any{"name": "title", "type": "string", "fulltext": true},
			map[string]any{"name": "body", "type": "text", "fulltext": true},
			map[string]any{"name": "embedding", "type": "vector", "dim": 8},
		},
	}
	events := map[string]any{
		"namespace": scenarioNamespace,
		"table":     "events",
		"fields": []any{
			map[string]any{"name": "kind", "type": "string"},
			map[string]any{"name": "detail", "type": "string"},
		},
	}

	for _, spec := range []map[string]any{tickets, kbArticles, events} {
		name := spec["table"].(string)
		data := op(t, "create_table", spec)
		table, _ := data["table"].(map[string]any)
		if table == nil {
			t.Fatalf("create_table %s: no table object in response", name)
		}
		assertConforms(t, openapiSchema(t, "TableSchema"), table, "create_table."+name)
		if asInt(t, table["version"], "table.version") != 1 {
			t.Fatalf("create_table %s: version does not start at 1: %v", name, table["version"])
		}
	}

	describeTickets := describeTable(t, "tickets")
	if rc := asInt(t, describeTickets["row_count"], "tickets row_count"); rc != 0 {
		t.Fatalf("tickets row_count after create: %d", rc)
	}
	ticketFields := fieldMap(t, describeTickets)
	statusField, ok := ticketFields["status"]
	if !ok {
		t.Fatalf("describe_table tickets: no status field: %v", describeTickets)
	}
	enum, _ := statusField["enum"].([]any)
	if len(enum) != 3 {
		t.Fatalf("describe_table tickets: status enum not reported: %v", statusField)
	}
	if fulltext, _ := ticketFields["subject"]["fulltext"].(bool); !fulltext {
		t.Fatalf("describe_table tickets: subject fulltext not reported: %v", ticketFields["subject"])
	}

	describeKB := describeTable(t, "kb_articles")
	kbFields := fieldMap(t, describeKB)
	embeddingField, ok := kbFields["embedding"]
	if !ok {
		t.Fatalf("describe_table kb_articles: no embedding field: %v", describeKB)
	}
	if dim := asInt(t, embeddingField["dim"], "kb_articles.embedding.dim"); dim != 8 {
		t.Fatalf("kb_articles embedding dim: %d", dim)
	}
	if fulltext, _ := kbFields["title"]["fulltext"].(bool); !fulltext {
		t.Fatalf("describe_table kb_articles: title fulltext not reported: %v", kbFields["title"])
	}

	describeEvents := describeTable(t, "events")
	if rc := asInt(t, describeEvents["row_count"], "events row_count"); rc != 0 {
		t.Fatalf("events row_count after create: %d", rc)
	}

	data := op(t, "list_tables", map[string]any{"namespace": scenarioNamespace})
	tables, _ := data["tables"].([]any)
	want := map[string]bool{"tickets": false, "kb_articles": false, "events": false}
	for _, name := range tables {
		s, _ := name.(string)
		if _, tracked := want[s]; tracked {
			want[s] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("list_tables does not report %s: %v", name, tables)
		}
	}
}

func describeTable(t *testing.T, table string) map[string]any {
	t.Helper()
	data := op(t, "describe_table", map[string]any{"namespace": scenarioNamespace, "table": table})
	tableObj, _ := data["table"].(map[string]any)
	if tableObj == nil {
		t.Fatalf("describe_table %s: no table object: %v", table, data)
	}
	assertConforms(t, openapiSchema(t, "TableSchema"), tableObj, "describe_table."+table)
	if _, ok := data["row_count"]; !ok {
		t.Fatalf("describe_table %s: no row_count: %v", table, data)
	}
	return data
}

func fieldMap(t *testing.T, describe map[string]any) map[string]map[string]any {
	t.Helper()
	tableObj, _ := describe["table"].(map[string]any)
	rawFields, _ := tableObj["fields"].([]any)
	out := map[string]map[string]any{}
	for _, f := range rawFields {
		field, _ := f.(map[string]any)
		name := asStr(t, field["name"], "field name")
		out[name] = field
	}
	return out
}
