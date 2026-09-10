package api

import (
	"net/http/httptest"
	"testing"
	"time"
)

// deadlineRecorder is a ResponseWriter that records per-write deadlines:
// http.NewResponseController finds its SetWriteDeadline directly, the same
// lookup path the handler's real writes take through the server's wrapper.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	set []time.Time
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.set = append(d.set, t)
	return nil
}

// TestSSEWritesAreDeadlineBounded: every SSE frame write carries a
// deadline while in flight — a subscriber that stops reading blocks a
// write on a full socket buffer indefinitely, and a stuck write holding
// the stream mutex would wedge the terminal and the handler (graceful
// shutdown included) behind it — and CLEARS it once the frame lands: a
// timer left armed fires on an idle stream (HTTP/2 resets it), and an
// expired deadline fails the next write even on HTTP/1.
func TestSSEWritesAreDeadlineBounded(t *testing.T) {
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	if !sseEvent(rec, "change", sseChange{Cursor: "c", Table: "t", RowID: 1, Kind: "insert"}) {
		t.Fatal("frame write failed")
	}
	if len(rec.set) < 2 {
		t.Fatalf("frame write set %d deadlines, want a bound before and a clear after: %v", len(rec.set), rec.set)
	}
	if !rec.set[0].After(time.Now()) {
		t.Fatal("the in-flight bound is not in the future")
	}
	if !rec.set[len(rec.set)-1].IsZero() {
		t.Fatal("the deadline was not cleared after the frame landed — an armed timer fires on an idle stream")
	}
}
