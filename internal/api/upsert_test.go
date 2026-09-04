package api

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/store"
)

func mustCreateUsers(t *testing.T, base string) {
	t.Helper()
	code, res := post(t, base, "create_table", map[string]any{
		"namespace": "app",
		"table":     "users",
		"fields": []map[string]any{
			{"name": "email", "type": "string"},
			{"name": "plan_name", "type": "string"},
			{"name": "logins", "type": "number"},
			{"name": "active", "type": "boolean"},
		},
	})
	if code != 200 || res["ok"] != true {
		t.Fatalf("create_table failed: %d %v", code, res)
	}
}

// dataKeys returns the response data's exact key set.
func dataKeys(t *testing.T, res map[string]any) map[string]bool {
	t.Helper()
	data, ok := res["data"].(map[string]any)
	if !ok {
		t.Fatalf("response must carry a data object, got %v", res)
	}
	keys := map[string]bool{}
	for k := range data {
		keys[k] = true
	}
	return keys
}

func keySet(keys ...string) map[string]bool {
	set := map[string]bool{}
	for _, k := range keys {
		set[k] = true
	}
	return set
}

// TestWriteOpsShareResponseShape pins the write contract: insert, upsert, and
// upsert_by_key answer in one shape — ids of the touched rows plus inserted
// everywhere; updated wherever a write can update; replayed only where
// idempotency applies (an idempotent insert), and never required.
func TestWriteOpsShareResponseShape(t *testing.T) {
	srv := newTestServer(t)
	mustCreateUsers(t, srv.URL)

	// plain insert: no update branch exists and without a key nothing replays
	code, res := post(t, srv.URL, "insert", map[string]any{
		"namespace": "app", "table": "users",
		"records": []map[string]any{{"email": "a@example.com", "plan_name": "free"}},
	})
	if code != 200 {
		t.Fatalf("plain insert failed: %d %v", code, res)
	}
	if got := dataKeys(t, res); !reflect.DeepEqual(got, keySet("ids", "inserted")) {
		t.Fatalf("plain insert keys = %v, want exactly {ids, inserted}", got)
	}

	// idempotent insert: replayed joins the shape on first call and replay alike
	body := map[string]any{
		"namespace": "app", "table": "users",
		"records":         []map[string]any{{"email": "b@example.com", "plan_name": "pro"}},
		"idempotency_key": "shape-1",
	}
	code, res = post(t, srv.URL, "insert", body)
	if code != 200 {
		t.Fatalf("idempotent insert failed: %d %v", code, res)
	}
	if got := dataKeys(t, res); !reflect.DeepEqual(got, keySet("ids", "inserted", "replayed")) {
		t.Fatalf("idempotent insert keys = %v, want exactly {ids, inserted, replayed}", got)
	}
	code, res = post(t, srv.URL, "insert", body)
	if code != 200 {
		t.Fatalf("replayed insert failed: %d %v", code, res)
	}
	data := res["data"].(map[string]any)
	if got := dataKeys(t, res); !reflect.DeepEqual(got, keySet("ids", "inserted", "replayed")) {
		t.Fatalf("replayed insert keys = %v, want exactly {ids, inserted, replayed}", got)
	}
	if data["inserted"].(float64) != 0 || data["replayed"] != true {
		t.Fatalf("replay must insert nothing and say so, got %v", data)
	}

	// upsert insert path: the shared three keys, ids carrying the new row
	code, res = post(t, srv.URL, "upsert", map[string]any{
		"namespace": "app", "table": "users",
		"filter": "email = 'c@example.com'",
		"set":    map[string]any{"email": "c@example.com", "plan_name": "free"},
	})
	if code != 200 {
		t.Fatalf("upsert insert failed: %d %v", code, res)
	}
	if got := dataKeys(t, res); !reflect.DeepEqual(got, keySet("ids", "inserted", "updated")) {
		t.Fatalf("upsert insert-path keys = %v, want exactly {ids, inserted, updated}", got)
	}
	data = res["data"].(map[string]any)
	if data["inserted"].(float64) != 1 || data["updated"].(float64) != 0 || len(data["ids"].([]any)) != 1 {
		t.Fatalf("upsert insert path must report one new row id, got %v", data)
	}
	rowID := data["ids"].([]any)[0].(float64)

	// upsert update path: same keys, ids carrying the updated row
	code, res = post(t, srv.URL, "upsert", map[string]any{
		"namespace": "app", "table": "users",
		"filter": "email = 'c@example.com'",
		"set":    map[string]any{"plan_name": "pro"},
	})
	if code != 200 {
		t.Fatalf("upsert update failed: %d %v", code, res)
	}
	if got := dataKeys(t, res); !reflect.DeepEqual(got, keySet("ids", "inserted", "updated")) {
		t.Fatalf("upsert update-path keys = %v, want exactly {ids, inserted, updated}", got)
	}
	data = res["data"].(map[string]any)
	if data["inserted"].(float64) != 0 || data["updated"].(float64) != 1 {
		t.Fatalf("upsert update path must report one update, got %v", data)
	}
	if got := data["ids"].([]any); len(got) != 1 || got[0].(float64) != rowID {
		t.Fatalf("upsert update path must report the updated row's id %v, got %v", rowID, got)
	}

	// upsert_by_key: the same three keys, ids still naming touched rows
	code, res = post(t, srv.URL, "upsert_by_key", map[string]any{
		"namespace": "app", "table": "users", "on": []string{"email"},
		"records": []map[string]any{{"email": "c@example.com", "logins": 3}},
	})
	if code != 200 {
		t.Fatalf("upsert_by_key failed: %d %v", code, res)
	}
	if got := dataKeys(t, res); !reflect.DeepEqual(got, keySet("ids", "inserted", "updated")) {
		t.Fatalf("upsert_by_key keys = %v, want exactly {ids, inserted, updated}", got)
	}
	data = res["data"].(map[string]any)
	if data["inserted"].(float64) != 0 || data["updated"].(float64) != 1 {
		t.Fatalf("upsert_by_key must report one update, got %v", data)
	}
	if got := data["ids"].([]any); len(got) != 1 || got[0].(float64) != rowID {
		t.Fatalf("upsert_by_key must report the updated row's id %v, got %v", rowID, got)
	}
}

func TestUpsertByKeyOverHTTP(t *testing.T) {
	srv := newTestServer(t)
	mustCreateUsers(t, srv.URL)

	code, res := post(t, srv.URL, "upsert_by_key", map[string]any{
		"namespace": "app", "table": "users", "on": []string{"email"},
		"records": []map[string]any{
			{"email": "a@example.com", "plan_name": "free", "logins": 1},
			{"email": "b@example.com", "plan_name": "pro", "logins": 1},
		},
	})
	if code != 200 || res["ok"] != true {
		t.Fatalf("first upsert failed: %d %v", code, res)
	}
	data := res["data"].(map[string]any)
	if data["inserted"].(float64) != 2 || data["updated"].(float64) != 0 {
		t.Fatalf("first upsert should insert both: %v", data)
	}
	firstIDs := data["ids"].([]any)

	code, res = post(t, srv.URL, "upsert_by_key", map[string]any{
		"namespace": "app", "table": "users", "on": []string{"email"},
		"records": []map[string]any{
			{"email": "a@example.com", "logins": 5},
		},
	})
	if code != 200 {
		t.Fatalf("second upsert failed: %d %v", code, res)
	}
	data = res["data"].(map[string]any)
	if data["inserted"].(float64) != 0 || data["updated"].(float64) != 1 {
		t.Fatalf("second upsert should update one: %v", data)
	}
	if got := data["ids"].([]any); got[0] != firstIDs[0] {
		t.Fatalf("update must report the existing row id: %v vs %v", got, firstIDs)
	}

	code, res = post(t, srv.URL, "query", map[string]any{
		"namespace": "app",
		"sql":       "SELECT plan_name, logins, count(*) OVER () AS n FROM users WHERE email = ?",
		"args":      []any{"a@example.com"},
	})
	if code != 200 {
		t.Fatalf("query failed: %d %v", code, res)
	}
	rows := res["data"].(map[string]any)["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("upsert must not duplicate the row: %v", rows)
	}
	row := rows[0].(map[string]any)
	if row["plan_name"] != "free" || row["logins"].(float64) != 5 {
		t.Fatalf("partial update should set logins and keep plan: %v", row)
	}
}

func TestInsertIdempotencyOverHTTP(t *testing.T) {
	srv := newTestServer(t)
	mustCreateUsers(t, srv.URL)

	body := map[string]any{
		"namespace": "app", "table": "users",
		"records":         []map[string]any{{"email": "a@example.com", "plan_name": "free"}},
		"idempotency_key": "retry-safe-1",
	}
	code, res := post(t, srv.URL, "insert", body)
	if code != 200 || res["ok"] != true {
		t.Fatalf("insert failed: %d %v", code, res)
	}
	data := res["data"].(map[string]any)
	if data["replayed"] != false || data["inserted"].(float64) != 1 {
		t.Fatalf("first insert must insert once and not report replayed: %v", data)
	}
	ids := data["ids"].([]any)

	code, res = post(t, srv.URL, "insert", body)
	if code != 200 {
		t.Fatalf("retried insert failed: %d %v", code, res)
	}
	data = res["data"].(map[string]any)
	if data["replayed"] != true || data["inserted"].(float64) != 0 {
		t.Fatalf("retry must replay without inserting: %v", data)
	}
	if got := data["ids"].([]any); got[0] != ids[0] {
		t.Fatalf("retry must return the original ids: %v vs %v", got, ids)
	}

	code, res = post(t, srv.URL, "query", map[string]any{
		"namespace": "app", "sql": "SELECT count(*) AS n FROM users",
	})
	if code != 200 {
		t.Fatalf("query failed: %d %v", code, res)
	}
	rows := res["data"].(map[string]any)["rows"].([]any)
	if rows[0].(map[string]any)["n"].(float64) != 1 {
		t.Fatalf("retried insert must not duplicate rows: %v", rows)
	}

	// Same key, different records: rejected rather than replayed.
	body["records"] = []map[string]any{{"email": "c@example.com", "plan_name": "pro"}}
	code, res = post(t, srv.URL, "insert", body)
	if code != 400 {
		t.Fatalf("key reuse for a different payload must 400, got %d %v", code, res)
	}

	// An explicitly empty or null key must not silently fall back to a plain
	// (non-idempotent) insert — a retried call would then duplicate rows.
	for _, badKey := range []any{"", nil} {
		code, res = post(t, srv.URL, "insert", map[string]any{
			"namespace": "app", "table": "users",
			"records":         []map[string]any{{"email": "d@example.com"}},
			"idempotency_key": badKey,
		})
		if code != 400 {
			t.Fatalf("idempotency_key %v must 400, got %d %v", badKey, code, res)
		}
	}
	code, res = post(t, srv.URL, "query", map[string]any{
		"namespace": "app", "sql": "SELECT count(*) AS n FROM users WHERE email = 'd@example.com'",
	})
	if code != 200 {
		t.Fatalf("query failed: %d %v", code, res)
	}
	rows = res["data"].(map[string]any)["rows"].([]any)
	if rows[0].(map[string]any)["n"].(float64) != 0 {
		t.Fatalf("rejected empty-key inserts must write nothing: %v", rows)
	}
}

func TestInsertIdempotencyKeySchemaParity(t *testing.T) {
	def, ok := Ops["insert"]
	if !ok {
		t.Fatal("insert op missing")
	}
	props := def.InputSchema["properties"].(map[string]any)
	key := props["idempotency_key"].(map[string]any)
	if key["type"] != "string" || key["minLength"] != 1 || key["maxLength"] != store.MaxIdempotencyKeyLen {
		t.Fatalf("idempotency_key must declare its bounds to match the store (1..%d), got %v", store.MaxIdempotencyKeyLen, key)
	}
	if key["pattern"] != fmt.Sprintf(`^[ -~]{1,%d}$`, store.MaxIdempotencyKeyLen) {
		t.Fatalf("idempotency_key must restrict to printable ASCII so schema chars and store bytes agree, got %v", key["pattern"])
	}

	srv := newTestServer(t)
	mustCreateUsers(t, srv.URL)
	code, res := post(t, srv.URL, "insert", map[string]any{
		"namespace": "app", "table": "users",
		"records":         []map[string]any{{"email": "long@example.com"}},
		"idempotency_key": strings.Repeat("k", store.MaxIdempotencyKeyLen+1),
	})
	if code != 400 {
		t.Fatalf("over-length key must 400, got %d %v", code, res)
	}
	// 100 emoji are 400 bytes: schema chars and store bytes must agree, so a
	// multi-byte key is rejected up front with the byte-count reason.
	code, res = post(t, srv.URL, "insert", map[string]any{
		"namespace": "app", "table": "users",
		"records":         []map[string]any{{"email": "emoji@example.com"}},
		"idempotency_key": strings.Repeat("😀", 100),
	})
	if code != 400 {
		t.Fatalf("multi-byte key exceeding the byte budget must 400, got %d %v", code, res)
	}
	errEnv, _ := res["error"].(map[string]any)
	msg, _ := errEnv["message"].(string)
	if !strings.Contains(msg, "bytes") {
		t.Fatalf("rejection should state the byte budget, got %v", res["error"])
	}
}

func TestUpsertByKeySchemaParity(t *testing.T) {
	def, ok := Ops["upsert_by_key"]
	if !ok {
		t.Fatal("upsert_by_key op missing")
	}
	props := def.InputSchema["properties"].(map[string]any)
	on := props["on"].(map[string]any)
	if on["minItems"] != 1 || on["maxItems"] != store.MaxKeyFields {
		t.Fatalf(`"on" must declare minItems 1 / maxItems %d to match the store bound, got %v`, store.MaxKeyFields, on)
	}
	if on["uniqueItems"] != true {
		t.Fatalf(`"on" must declare uniqueItems to match duplicate-key-field rejection, got %v`, on)
	}
	if _, ok := on["items"].(map[string]any)["pattern"]; !ok {
		t.Fatalf(`"on" items must carry the field-name pattern, got %v`, on["items"])
	}
	records := props["records"].(map[string]any)
	if records["maxItems"] != store.MaxRecordsPerInsert {
		t.Fatalf(`"records" must declare maxItems %d to match the store bound, got %v`, store.MaxRecordsPerInsert, records)
	}
	if def.InputSchema["additionalProperties"] != false {
		t.Fatal("upsert_by_key must reject unknown properties")
	}
}

func TestUpsertByKeyValidationOverHTTP(t *testing.T) {
	srv := newTestServer(t)
	mustCreateUsers(t, srv.URL)

	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{
			name: "key field outside the schema",
			body: map[string]any{"namespace": "app", "table": "users", "on": []string{"nope"},
				"records": []map[string]any{{"nope": "x"}}},
			want: "not a field",
		},
		{
			name: "missing key value",
			body: map[string]any{"namespace": "app", "table": "users", "on": []string{"email"},
				"records": []map[string]any{{"plan_name": "free"}}},
			want: "must be present and non-null",
		},
	}
	for _, tc := range cases {
		code, res := post(t, srv.URL, "upsert_by_key", tc.body)
		if code != 400 {
			t.Fatalf("%s: expected 400, got %d %v", tc.name, code, res)
		}
		errEnv, _ := res["error"].(map[string]any)
		msg, _ := errEnv["message"].(string)
		if !strings.Contains(msg, tc.want) {
			t.Fatalf("%s: error should mention %q, got %v", tc.name, tc.want, res["error"])
		}
	}
}
