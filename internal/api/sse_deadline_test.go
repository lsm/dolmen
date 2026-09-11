package api

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

type deadlineSpy struct {
	header http.Header
	bytes  int
	calls  []time.Time
}

func (s *deadlineSpy) Header() http.Header {
	if s.header == nil {
		s.header = http.Header{}
	}
	return s.header
}

func (s *deadlineSpy) WriteHeader(int) {}

func (s *deadlineSpy) Write(p []byte) (int, error) {
	s.bytes += len(p)
	return len(p), nil
}

func (s *deadlineSpy) Flush() {}

func (s *deadlineSpy) SetWriteDeadline(t time.Time) error {
	s.calls = append(s.calls, t)
	return nil
}

type failingWriter struct {
	deadlineSpy
}

func (s *failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("peer reset")
}

func TestSSEWriteDeadlineIsScopedToEachWrite(t *testing.T) {
	sp := &deadlineSpy{}
	before := time.Now()

	sseOpenStream(sp, "req-1")
	if !sseEvent(sp, "change", sseChange{Cursor: "c1", Table: "notes", RowID: 1, Kind: "insert"}) {
		t.Fatal("the change frame failed on a healthy writer")
	}
	if !sseEvent(sp, "error", map[string]any{"ok": false}) {
		t.Fatal("the error frame failed on a healthy writer")
	}

	if sp.bytes == 0 {
		t.Fatal("the spy captured no written bytes")
	}
	if len(sp.calls) != 6 {
		t.Fatalf("3 writes produced %d SetWriteDeadline calls, want 6 (one arm and one disarm per write): %v", len(sp.calls), sp.calls)
	}
	for i, at := range sp.calls {
		if i%2 == 0 {
			if at.IsZero() || !at.After(before) {
				t.Fatalf("write %d armed %v, want a future deadline past %v", i/2, at, before)
			}
		} else if !at.IsZero() {
			t.Fatalf("write %d left %v armed after its flush, want the zero value — an idle stream must carry no deadline", (i-1)/2, at)
		}
	}
}

func TestSSEWriteDeadlineIsClearedWhenTheWriteFails(t *testing.T) {
	fw := &failingWriter{}
	before := time.Now()
	if sseEvent(fw, "change", sseChange{Cursor: "c1"}) {
		t.Fatal("the write reported success on a failing writer")
	}
	if len(fw.calls) != 2 || !fw.calls[0].After(before) || !fw.calls[1].IsZero() {
		t.Fatalf("a failed write produced %v, want one arm then the zero value — the failed frame must not leave a deadline behind", fw.calls)
	}
}
