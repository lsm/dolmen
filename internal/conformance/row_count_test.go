package conformance

import (
	"net/http"
	"testing"
)

func describedRows(t *testing.T, h *harness, ns, table string) int64 {
	t.Helper()
	data := h.mustHTTP("describe_table", map[string]any{"namespace": ns, "table": table})
	return int64val(t, "row_count", data["row_count"])
}

func scannedRows(t *testing.T, h *harness, ns, table string) int64 {
	t.Helper()
	data := h.mustHTTP("query", map[string]any{"namespace": ns, "sql": "SELECT count(*) AS n FROM " + table})
	rows, _ := data["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("count query returned %v", data)
	}
	row, _ := rows[0].(map[string]any)
	return int64val(t, "n", row["n"])
}

func wantRowCount(t *testing.T, h *harness, step string, want int64) {
	t.Helper()
	if got := scannedRows(t, h, "inv", "items"); got != want {
		t.Fatalf("%s: the table holds %d rows, the fixture expected %d", step, got, want)
	}
	if got := describedRows(t, h, "inv", "items"); got != want {
		t.Fatalf("%s: describe_table reported row_count %d, but the table holds %d", step, got, want)
	}
}

func TestRowCountFollowsEveryWritePath(t *testing.T) {
	h := newHarness(t)
	create := func() {
		h.mustHTTP("create_table", map[string]any{
			"namespace": "inv",
			"table":     "items",
			"fields": []map[string]any{
				{"name": "sku", "type": "string"},
				{"name": "qty", "type": "number"},
				{"name": "note", "type": "text", "fulltext": true},
			},
		})
	}
	create()
	wantRowCount(t, h, "a new table", 0)

	insert := map[string]any{"namespace": "inv", "table": "items", "idempotency_key": "batch-1", "records": []map[string]any{
		{"sku": "a", "qty": 1, "note": "alpha"}, {"sku": "b", "qty": 2, "note": "beta"}, {"sku": "c", "qty": 3, "note": "gamma"},
	}}
	h.mustHTTP("insert", insert)
	wantRowCount(t, h, "a three-record insert", 3)
	h.mustHTTP("insert", insert)
	wantRowCount(t, h, "an idempotent replay", 3)

	h.mustHTTP("upsert_by_key", map[string]any{"namespace": "inv", "table": "items", "on": []string{"sku"}, "records": []map[string]any{
		{"sku": "a", "qty": 10}, {"sku": "d", "qty": 4},
	}})
	wantRowCount(t, h, "an upsert_by_key matching one record and inserting one", 4)

	h.mustHTTP("upsert", map[string]any{"namespace": "inv", "table": "items", "filter": "sku = ?", "args": []any{"e"}, "set": map[string]any{"sku": "e", "qty": 5}})
	wantRowCount(t, h, "an upsert that inserts", 5)
	h.mustHTTP("upsert", map[string]any{"namespace": "inv", "table": "items", "filter": "sku = ?", "args": []any{"e"}, "set": map[string]any{"qty": 6}})
	wantRowCount(t, h, "an upsert that updates", 5)

	h.mustHTTP("update", map[string]any{"namespace": "inv", "table": "items", "filter": "qty > ?", "args": []any{0}, "set": map[string]any{"note": "restocked"}})
	wantRowCount(t, h, "an update of every row", 5)

	h.mustHTTP("delete", map[string]any{"namespace": "inv", "table": "items", "filter": "sku IN (?, ?)", "args": []any{"b", "c"}})
	wantRowCount(t, h, "a filtered delete of two rows", 3)

	h.mustHTTP("migrate", map[string]any{"namespace": "inv", "table": "items", "changes": []map[string]any{
		{"op": "add_field", "field": map[string]any{"name": "bin", "type": "string"}, "default": "A1"},
	}})
	wantRowCount(t, h, "an add_field backfill", 3)
	data := h.mustHTTP("describe_table", map[string]any{"namespace": "inv", "table": "items"})
	table, _ := data["table"].(map[string]any)
	h.mustHTTP("migrate", map[string]any{"namespace": "inv", "table": "items", "expected_version": table["version"], "changes": []map[string]any{
		{"op": "drop_field", "name": "note"},
	}})
	wantRowCount(t, h, "a drop_field", 3)

	h.mustHTTP("drop_table", map[string]any{"namespace": "inv", "table": "items", "confirm": "items"})
	create()
	wantRowCount(t, h, "a table recreated under a dropped table's name", 0)
	h.mustHTTP("insert", map[string]any{"namespace": "inv", "table": "items", "records": []map[string]any{{"sku": "z"}}})
	wantRowCount(t, h, "the recreated table's first insert", 1)

	status, out := h.httpCall("drop_namespace", map[string]any{"namespace": "inv", "confirm": "inv"})
	if status != http.StatusOK {
		t.Fatalf("drop_namespace: status %d %v", status, out)
	}
	create()
	wantRowCount(t, h, "a table recreated in a recreated namespace", 0)
}

func TestRowCountIsPerOwnerOnARowAccessTable(t *testing.T) {
	h := seedRowAccess(t)
	for _, who := range []string{"alice", "bob"} {
		grantTo(t, h, "principal", who, "acme", "notes", "create", "delete")
	}
	grantTo(t, h, "principal", "carol", "acme", "notes", "read")
	h.asIdentity(t, "alice", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"a1"},{"body":"a2"},{"body":"a3"}]}`)
	h.asIdentity(t, "bob", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"b1"}]}`)
	h.asIdentity(t, "alice", "", "delete", `{"namespace":"acme","table":"notes","filter":"body = 'a1'"}`)

	for who, want := range map[string]int64{"alice": 2, "bob": 1, "carol": 3} {
		res, out := h.asIdentity(t, who, "", "describe_table", `{"namespace":"acme","table":"notes"}`)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s describe_table: status %d %v", who, res.StatusCode, out)
		}
		data, _ := out["data"].(map[string]any)
		if got := int64val(t, "row_count", data["row_count"]); got != want {
			t.Fatalf("%s saw row_count %d, want %d", who, got, want)
		}
	}
}

func TestRowCountIsPerOwnerAfterRowAccessIsAdopted(t *testing.T) {
	h := newHarnessMode(t, authGateway)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	h.mustHTTP("create_table", map[string]any{"namespace": "acme", "table": "notes", "fields": []map[string]any{{"name": "body", "type": "text"}}})
	h.mustHTTP("migrate", map[string]any{"namespace": "acme", "table": "notes", "changes": []map[string]any{{"op": "set_row_access", "value": true}}})
	grantTo(t, h, "principal", "alice", "acme", "notes", "create")
	grantTo(t, h, "principal", "bob", "acme", "notes", "create")
	h.asIdentity(t, "alice", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"a1"},{"body":"a2"}]}`)
	h.asIdentity(t, "bob", "", "insert", `{"namespace":"acme","table":"notes","records":[{"body":"b1"}]}`)

	for who, want := range map[string]int64{"alice": 2, "bob": 1} {
		res, out := h.asIdentity(t, who, "", "describe_table", `{"namespace":"acme","table":"notes"}`)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s describe_table: status %d %v", who, res.StatusCode, out)
		}
		data, _ := out["data"].(map[string]any)
		if got := int64val(t, "row_count", data["row_count"]); got != want {
			t.Fatalf("%s saw row_count %d on a table that adopted row_access, want %d", who, got, want)
		}
	}
	if got := describedRows(t, h, "acme", "notes"); got != 3 {
		t.Fatalf("the administrator saw row_count %d, want 3", got)
	}
}
