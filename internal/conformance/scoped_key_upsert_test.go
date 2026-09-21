package conformance

import (
	"encoding/json"
	"net/http"
	"testing"
)

func seedKeyedRowAccess(t *testing.T) *harness {
	t.Helper()
	h := newHarnessMode(t, authGateway)
	h.mustHTTP("create_namespace", map[string]any{"namespace": "acme"})
	h.mustHTTP("create_table", map[string]any{
		"namespace": "acme",
		"table":     "notes",
		"fields": []map[string]any{
			{"name": "slug", "type": "string"},
			{"name": "body", "type": "text"},
		},
		"row_access": "own",
	})
	for _, who := range []string{"alice", "bob"} {
		grantTo(t, h, "principal", who, "acme", "notes", "create", "update")
	}
	return h
}

func upsertSlug(t *testing.T, h *harness, who, slug, body string) map[string]any {
	t.Helper()
	res, out := h.asIdentity(t, who, "", "upsert_by_key",
		`{"namespace":"acme","table":"notes","on":["slug"],"records":[{"slug":"`+slug+`","body":"`+body+`"}]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("%s upsert_by_key %q: status %d %v", who, slug, res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	return data
}

func countsOf(t *testing.T, data map[string]any) (inserted, updated float64) {
	t.Helper()
	inserted, _ = data["inserted"].(float64)
	updated, _ = data["updated"].(float64)
	return inserted, updated
}

func TestAnInvisibleKeyMatchIsNoMatch(t *testing.T) {
	h := seedKeyedRowAccess(t)
	bobs := upsertSlug(t, h, "bob", "shared", "written by bob")
	bobIDs, _ := bobs["ids"].([]any)
	if len(bobIDs) != 1 {
		t.Fatalf("bob's insert returned %v", bobs["ids"])
	}

	taken := upsertSlug(t, h, "alice", "shared", "written by alice")
	if inserted, updated := countsOf(t, taken); inserted != 1 || updated != 0 {
		t.Fatalf("bob's row holds the key but alice cannot see it, so her upsert must insert: inserted %v updated %v", inserted, updated)
	}

	id, err := json.Marshal(bobIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	res, out := h.asIdentity(t, "bob", "", "read_rows",
		`{"namespace":"acme","table":"notes","ids":[`+string(id)+`]}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("bob read_rows: status %d %v", res.StatusCode, out)
	}
	data, _ := out["data"].(map[string]any)
	rows, _ := data["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("bob's own row went missing: %v", rows)
	}
	row, _ := rows[0].(map[string]any)
	if row["body"] != "written by bob" {
		t.Fatalf("alice's upsert reached through the scope and rewrote bob's row: %v", row)
	}
}

func TestATakenKeyAnswersExactlyLikeAFreeOne(t *testing.T) {
	h := seedKeyedRowAccess(t)
	upsertSlug(t, h, "bob", "taken", "written by bob")

	onTaken := upsertSlug(t, h, "alice", "taken", "written by alice")
	onFree := upsertSlug(t, h, "alice", "free", "written by alice")

	delete(onTaken, "ids")
	delete(onFree, "ids")
	delete(onTaken, "changes")
	delete(onFree, "changes")

	takenJSON, err := json.Marshal(onTaken)
	if err != nil {
		t.Fatal(err)
	}
	freeJSON, err := json.Marshal(onFree)
	if err != nil {
		t.Fatal(err)
	}
	if string(takenJSON) != string(freeJSON) {
		t.Fatalf("the response tells alice that someone else holds the key:\n taken: %s\n free:  %s", takenJSON, freeJSON)
	}
}

func TestAVisibleKeyMatchStillUpdates(t *testing.T) {
	h := seedKeyedRowAccess(t)
	first := upsertSlug(t, h, "alice", "mine", "first body")
	if inserted, updated := countsOf(t, first); inserted != 1 || updated != 0 {
		t.Fatalf("the first upsert of a fresh key inserts: inserted %v updated %v", inserted, updated)
	}

	second := upsertSlug(t, h, "alice", "mine", "second body")
	if inserted, updated := countsOf(t, second); inserted != 0 || updated != 1 {
		t.Fatalf("alice can see her own row, so her second upsert must update it rather than insert a twin: inserted %v updated %v", inserted, updated)
	}

	firstIDs, _ := first["ids"].([]any)
	secondIDs, _ := second["ids"].([]any)
	if len(firstIDs) != 1 || len(secondIDs) != 1 || firstIDs[0] != secondIDs[0] {
		t.Fatalf("the update must land on the row the insert created: %v then %v", firstIDs, secondIDs)
	}
}
