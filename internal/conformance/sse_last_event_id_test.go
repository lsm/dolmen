package conformance

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func (h *harness) subscribeStreamWithHeader(t *testing.T, query url.Values, header http.Header) *sseReader {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(h.api.HandleSubscribe))
	t.Cleanup(srv.Close)
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/v1/subscribe?"+query.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = header
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open subscribe stream: %v", err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return newSSEReader(res)
}

func TestSubscribeFramesCarryTheirCursorAsEventID(t *testing.T) {
	h := newHarness(t)
	h.seedTable("lei", "notes", []map[string]any{{"name": "title", "type": "string"}})
	ids := h.mustHTTP("insert", map[string]any{"namespace": "lei", "table": "notes", "records": []any{
		map[string]any{"title": "a"}, map[string]any{"title": "b"}, map[string]any{"title": "c"},
	}})["ids"].([]any)

	r := h.subscribeStream(t, url.Values{"namespace": {"lei"}, "cursor": {"begin"}})
	var changes []sseFrame
	for len(changes) < 3 {
		f, ok := r.next(5 * time.Second)
		if !ok {
			t.Fatalf("replay delivered only %d of 3 changes", len(changes))
		}
		if f.event != "change" {
			continue
		}
		cursor, _ := frameData(t, f)["cursor"].(string)
		if f.id == "" || f.id != cursor {
			t.Fatalf("a change frame must carry its cursor as the SSE id: id %q, cursor %q", f.id, cursor)
		}
		changes = append(changes, f)
	}
	ready, ok := r.next(5 * time.Second)
	if !ok {
		t.Fatal("the stream never sent its ready frame")
	}
	if c := wantReady(t, ready); ready.id != c {
		t.Fatalf("the ready frame must carry its cursor as the SSE id: id %q, cursor %q", ready.id, c)
	}

	header := http.Header{"Last-Event-ID": {changes[1].id}}
	r2 := h.subscribeStreamWithHeader(t, url.Values{"namespace": {"lei"}, "cursor": {"begin"}}, header)
	f, ok := r2.next(5 * time.Second)
	if !ok {
		t.Fatal("the resumed stream delivered nothing")
	}
	wantChange(t, f, [3]any{"notes", ids[2], "insert"})
	next, ok := r2.next(5 * time.Second)
	if !ok {
		t.Fatal("the resumed stream never sent its ready frame")
	}
	wantReady(t, next)
}
