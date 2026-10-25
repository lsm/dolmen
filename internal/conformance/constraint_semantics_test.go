package conformance

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func (h *harness) mustConflict(op string, body map[string]any, name string) {
	h.t.Helper()
	code, out := h.httpCall(op, body)
	if code != http.StatusBadRequest || errCode(out) != "conflict" || !strings.Contains(errMessage(out), `constraint "`+name+`"`) {
		h.t.Fatalf("%s must conflict on %s: %d %v", op, name, code, out)
	}
}

func keyTable(h *harness, fields []map[string]any, keys []string, where string) {
	h.t.Helper()
	c := map[string]any{"name": "row_key", "kind": "unique", "fields": keys}
	if where != "" {
		c["where"] = where
	}
	h.mustHTTP("create_namespace", map[string]any{"namespace": "app"})
	h.mustHTTP("create_table", map[string]any{"namespace": "app", "table": "items", "fields": fields, "constraints": []map[string]any{c}})
}

func insertItems(h *harness, recs ...map[string]any) (int, map[string]any) {
	return h.httpCall("insert", map[string]any{"namespace": "app", "table": "items", "records": recs})
}

func TestUniqueKeysCompareTypedValuesExactly(t *testing.T) {
	h := newHarness(t)
	keyTable(h, []map[string]any{{"name": "n", "type": "number"}, {"name": "observed_at", "type": "timestamp"}, {"name": "tag", "type": "string"}}, []string{"n", "observed_at", "tag"}, "")
	h.mustHTTP("insert", map[string]any{"namespace": "app", "table": "items", "records": []map[string]any{
		{"n": json.Number("9223372036854775807"), "observed_at": "2026-10-23T12:00:00Z", "tag": "a"},
		{"n": json.Number("9223372036854775806"), "observed_at": "2026-10-23T12:00:00Z", "tag": "a"},
		{"n": 1, "observed_at": "2026-10-23T08:00:00-04:00", "tag": "a"},
		{"n": 1, "observed_at": "2026-10-23T12:00:00.000Z", "tag": "a"},
		{"n": 1, "observed_at": "2026-10-23T12:00:00Z", "tag": "a"},
		{"n": 1, "observed_at": "2026-10-23T12:00:00Z", "tag": "A"},
		{"n": 1, "observed_at": "2026-10-23T12:00:00Z", "tag": nil},
		{"n": 1, "observed_at": "2026-10-23T12:00:00Z", "tag": nil},
	}})
	h.mustConflict("insert", map[string]any{"namespace": "app", "table": "items", "records": []map[string]any{{"n": json.Number("1.0"), "observed_at": "2026-10-23T08:00:00-04:00", "tag": "a"}}}, "row_key")
	h.mustConflict("insert", map[string]any{"namespace": "app", "table": "items", "records": []map[string]any{{"n": json.Number("9223372036854775807"), "observed_at": "2026-10-23T12:00:00Z", "tag": "a"}}}, "row_key")
}

func TestPartialUniqueKeysFollowPredicateTransitions(t *testing.T) {
	h := newHarness(t)
	keyTable(h, []map[string]any{{"name": "slug", "type": "string"}, {"name": "retired_at", "type": "timestamp"}}, []string{"slug"}, "retired_at IS NULL")
	h.mustHTTP("insert", map[string]any{"namespace": "app", "table": "items", "records": []map[string]any{{"slug": "a"}, {"slug": "a", "retired_at": "2026-01-01T00:00:00Z"}}})
	h.mustConflict("update", map[string]any{"namespace": "app", "table": "items", "filter": "id = 2", "set": map[string]any{"retired_at": nil}}, "row_key")
	h.mustHTTP("update", map[string]any{"namespace": "app", "table": "items", "filter": "id = 1", "set": map[string]any{"retired_at": "2026-02-01T00:00:00Z"}})
	h.mustHTTP("update", map[string]any{"namespace": "app", "table": "items", "filter": "id = 2", "set": map[string]any{"retired_at": nil}})
}

func TestAViolationRollsBackTheWholeWriteAndBatch(t *testing.T) {
	h := newHarness(t)
	keyTable(h, []map[string]any{{"name": "slug", "type": "string"}}, []string{"slug"}, "")
	if code, out := insertItems(h, map[string]any{"slug": "x"}, map[string]any{"slug": "x"}); code != http.StatusBadRequest || errCode(out) != "conflict" {
		t.Fatalf("duplicates within one insert: %d %v", code, out)
	}
	h.mustHTTP("insert", map[string]any{"namespace": "app", "table": "items", "records": []map[string]any{{"slug": "kept"}}})
	h.mustConflict("batch", map[string]any{"namespace": "app", "writes": []map[string]any{
		{"kind": "insert", "table": "items", "records": []map[string]any{{"slug": "first"}}},
		{"kind": "insert", "table": "items", "records": []map[string]any{{"slug": "kept"}}},
	}}, "row_key")
	rows := h.mustHTTP("read_rows", map[string]any{"namespace": "app", "table": "items"})["rows"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["slug"] != "kept" {
		t.Fatalf("a refused write or batch left rows behind: %v", rows)
	}
	changes := h.mustHTTP("changes_since", map[string]any{"namespace": "app", "table": "items", "cursor": "begin"})["changes"].([]any)
	if len(changes) != 1 {
		t.Fatalf("a refused write or batch left change-feed records: %v", changes)
	}
}

func TestConcurrentDuplicatesCommitExactlyOne(t *testing.T) {
	h := newHarness(t)
	keyTable(h, []map[string]any{{"name": "slug", "type": "string"}}, []string{"slug"}, "")
	var wg sync.WaitGroup
	codes := make([]int, 8)
	errors := make([]string, len(codes))
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, out := insertItems(h, map[string]any{"slug": "race"})
			codes[i], errors[i] = code, errCode(out)
		}()
	}
	wg.Wait()
	ok := 0
	for i, c := range codes {
		if c == http.StatusOK {
			ok++
		} else if c != http.StatusBadRequest || errors[i] != "conflict" {
			t.Fatalf("a racing duplicate must be a conflict: status %d code %s", c, errors[i])
		}
	}
	rows := h.mustHTTP("read_rows", map[string]any{"namespace": "app", "table": "items"})["rows"].([]any)
	if ok != 1 || len(rows) != 1 {
		t.Fatalf("racing duplicates: %d succeeded, %d rows stored (codes %v)", ok, len(rows), codes)
	}
}

func TestConstraintsSurviveARestart(t *testing.T) {
	h := newHarness(t)
	keyTable(h, []map[string]any{{"name": "slug", "type": "string"}}, []string{"slug"}, "")
	h.mustHTTP("insert", map[string]any{"namespace": "app", "table": "items", "records": []map[string]any{{"slug": "x"}}})
	h.reopen()
	h.mustConflict("insert", map[string]any{"namespace": "app", "table": "items", "records": []map[string]any{{"slug": "x"}}}, "row_key")
}
