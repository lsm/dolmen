package blackbox

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStage04AssistantLoopOverHTTP(t *testing.T) {
	ft := op(t, "search_fulltext", map[string]any{
		"namespace": scenarioNamespace,
		"table":     "kb_articles",
		"query":     "refund policy",
		"limit":     5,
	})
	results := asMapList(t, ft["results"], "fulltext results")
	if len(results) == 0 {
		t.Fatal("full-text search for 'refund policy' returned nothing")
	}
	if truncated, _ := ft["truncated"].(bool); truncated {
		t.Fatalf("fulltext page truncated: %v", ft)
	}
	if title := asStr(t, results[0]["title"], "ft title"); !strings.Contains(strings.ToLower(title), "refund") {
		t.Fatalf("top fulltext hit for 'refund policy' is %v", results[0])
	}

	sem := op(t, "search_vector", map[string]any{
		"namespace": scenarioNamespace,
		"table":     "kb_articles",
		"vector":    embedText("how do I cancel"),
		"limit":     40,
	})
	semResults := asMapList(t, sem["results"], "semantic results")
	if len(semResults) < len(app.kbCancelID) {
		t.Fatalf("semantic search returned %d results, fewer than the cancel-axis articles", len(semResults))
	}
	cancelSet := map[int64]bool{}
	for _, id := range app.kbCancelID {
		cancelSet[id] = true
	}
	for i := 0; i < len(app.kbCancelID); i++ {
		id := asInt(t, semResults[i]["id"], "semantic result id")
		if !cancelSet[id] {
			t.Fatalf("semantic position %d for 'how do I cancel' is row %d, not a cancellation article; ranking: %v", i, id, semResults)
		}
	}
	prev := 2.0
	for i, r := range semResults {
		raw, ok := r["_score"]
		if !ok {
			t.Fatalf("semantic result %d carries no _score: %v", i, r)
		}
		s, ok := raw.(float64)
		if !ok {
			t.Fatalf("semantic result %d has a non-numeric _score: %v", i, r)
		}
		if s > prev+1e-9 {
			t.Fatalf("semantic results are not ordered by descending _score at position %d", i)
		}
		prev = s
	}
	if _, ok := sem["skipped_vectors"]; !ok {
		t.Fatalf("search_vector response does not report skipped_vectors: %v", sem)
	}

	ticket := func() map[string]any {
		return map[string]any{
			"namespace":       scenarioNamespace,
			"table":           "tickets",
			"idempotency_key": "file-ticket-refund",
			"records": []any{map[string]any{
				"subject": "Customer asks for a refund after 40 days",
				"body":    "Referenced the refund policy article and wants money back",
				"status":  "open",
			}},
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		payload, err := json.Marshal(ticket())
		if err != nil {
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, app.srv.url+"/v1/insert", bytes.NewReader(payload))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{}).Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()
	time.AfterFunc(10*time.Millisecond, cancel)
	wg.Wait()

	filedData := op(t, "insert", ticket())
	filedIDs, _ := filedData["ids"].([]any)
	if len(filedIDs) != 1 {
		t.Fatalf("filing the ticket after the killed write returned %v", filedData)
	}
	ticketID := asInt(t, filedIDs[0], "filed ticket id")

	counts := op(t, "query", map[string]any{
		"namespace": scenarioNamespace,
		"sql":       "SELECT COUNT(*) AS n FROM tickets WHERE subject = ?",
		"args":      []any{"Customer asks for a refund after 40 days"},
	})
	rows := asMapList(t, counts["rows"], "ticket count rows")
	if n := asInt(t, rows[0]["n"], "ticket count"); n != 1 {
		t.Fatalf("the killed write plus its retry left %d tickets with the same idempotency key, expected exactly 1", n)
	}

	op(t, "insert", map[string]any{
		"namespace": scenarioNamespace,
		"table":     "events",
		"records": []any{
			map[string]any{"kind": "file_ticket", "detail": "file-ticket-refund"},
			map[string]any{"kind": "search", "detail": "refund policy then how do I cancel"},
		},
	})

	flipped := op(t, "upsert_by_key", map[string]any{
		"namespace": scenarioNamespace,
		"table":     "tickets",
		"on":        []any{"subject"},
		"records": []any{map[string]any{
			"subject": "Customer asks for a refund after 40 days",
			"status":  "triaged",
		}},
	})
	if updated := asInt(t, flipped["updated"], "upsert updated"); updated != 1 {
		t.Fatalf("upsert_by_key did not update the matched ticket: %v", flipped)
	}
	if inserted := asInt(t, flipped["inserted"], "upsert inserted"); inserted != 0 {
		t.Fatalf("upsert_by_key inserted a second ticket: %v", flipped)
	}
	flipIDs, _ := flipped["ids"].([]any)
	if len(flipIDs) != 1 || asInt(t, flipIDs[0], "upsert id") != ticketID {
		t.Fatalf("upsert_by_key converged on %v, not the filed ticket %d", flipIDs, ticketID)
	}
	op(t, "insert", map[string]any{
		"namespace": scenarioNamespace,
		"table":     "events",
		"records": []any{
			map[string]any{"kind": "flip_status", "detail": "open to triaged"},
		},
	})

	state := op(t, "query", map[string]any{
		"namespace": scenarioNamespace,
		"sql":       "SELECT id, status FROM tickets",
	})
	tix := asMapList(t, state["rows"], "tickets rows")
	if len(tix) != 1 {
		t.Fatalf("tickets table holds %d rows after the loop, expected 1", len(tix))
	}
	if s := asStr(t, tix[0]["status"], "ticket status"); s != "triaged" {
		t.Fatalf("ticket status after upsert_by_key: %q", s)
	}
	ev := op(t, "query", map[string]any{
		"namespace": scenarioNamespace,
		"sql":       "SELECT COUNT(*) AS n FROM events",
	})
	evRows := asMapList(t, ev["rows"], "events count rows")
	if n := asInt(t, evRows[0]["n"], "events count"); n != 3 {
		t.Fatalf("telemetry holds %d events, expected one per assistant action", n)
	}
}
