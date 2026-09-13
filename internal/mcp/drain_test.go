package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestRequestIDKeyNormalization(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{`"a"`, `"a"`},
		{`"a"`, `"a"`},
		{`"A"`, `"A"`},
		{`"e-wait"`, `"e-wait"`},
		{`1`, `1`},
		{`1.0`, `1`},
		{`1e0`, `1`},
		{` 1 `, `1`},
		{`1.50`, `1.5`},
		{`0`, `0`},
		{`-0.0`, `0`},
		{`1e-400`, `0`},
		{`1000000`, `1000000`},
		{`1000000.0`, `1000000`},
		{`1e6`, `1000000`},
		{`-2e6`, `-2000000`},
		{`-2`, `-2`},
		{`9007199254740993`, `9007199254740993`},
		{`9007199254740993.0`, `9007199254740992`},
		{`9007199254740994.0`, `9007199254740994`},
		{`-9223372036854775808.0`, `-9223372036854775808`},
		{`9223372036854775808`, `9223372036854775808`},
		{`9223372036854775808.0`, `9.223372036854776e+18`},
		{`18446744073709551615`, `18446744073709551615`},
		{`18446744073709551616`, `1.8446744073709552e+19`},
		{`12345678901234567890`, `12345678901234567890`},
		{`12345678901234567891`, `12345678901234567891`},
		{`1e400`, `1e400`},
		{`"1"`, `"1"`},
		{`true`, ``},
		{`null`, ``},
		{``, ``},
		{`{`, ``},
	}
	for _, tc := range cases {
		if got := requestIDKey([]byte(tc.raw)); got != tc.want {
			t.Errorf("requestIDKey(%s) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestCancelledRequestKey(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{`{"requestId":"e-wait"}`, `"e-wait"`},
		{`{"requestId": 3 }`, `3`},
		{`{"requestId":"x","extra":true}`, `"x"`},
		{`{}`, ``},
		{`{"requestId":null}`, ``},
		{`not json`, ``},
	}
	for _, tc := range cases {
		if got := cancelledRequestKey([]byte(tc.raw)); got != tc.want {
			t.Errorf("cancelledRequestKey(%s) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

type fakeWorker struct {
	started  chan struct{}
	released chan struct{}
}

func newFakeWorker() *fakeWorker {
	return &fakeWorker{
		started:  make(chan struct{}),
		released: make(chan struct{}),
	}
}

func (w *fakeWorker) immediately(context.Context) {
	close(w.started)
}

func (w *fakeWorker) exitOnCancel(ctx context.Context) {
	close(w.started)
	<-ctx.Done()
}

func (w *fakeWorker) ignoreCancel(context.Context) {
	close(w.started)
	<-w.released
}

func awaitStarted(t *testing.T, workers ...*fakeWorker) {
	t.Helper()
	for _, w := range workers {
		select {
		case <-w.started:
		case <-time.After(2 * time.Second):
			t.Fatal("worker did not start within 2s")
		}
	}
}

func TestInflightCancelKeyNormalizesSpellings(t *testing.T) {
	for _, tc := range []struct{ registered, cancelWith string }{
		{`"e-wait"`, `"e-wait"`},
		{` 1e0 `, `1`},
		{`"1"`, `"1"`},
	} {
		r := newInflightRequests()
		w := newFakeWorker()
		r.start(context.Background(), json.RawMessage(tc.registered), w.exitOnCancel)
		awaitStarted(t, w)
		if !r.cancelKey(tc.cancelWith) {
			t.Fatalf("cancelKey(%s) must reach the request registered as %s", tc.cancelWith, tc.registered)
		}
		if r.cancelKey(`"never-registered"`) {
			t.Fatal("cancelKey must report an unknown key")
		}
		if !r.drain(context.Background(), 2*time.Second, 2*time.Second) {
			t.Fatalf("drain must join the cancelled worker registered as %s", tc.registered)
		}
	}
}

func TestInflightDuplicateIDKeepsEveryWorkerReachable(t *testing.T) {
	r := newInflightRequests()
	first, second := newFakeWorker(), newFakeWorker()
	r.start(context.Background(), json.RawMessage(`"dup"`), first.exitOnCancel)
	awaitStarted(t, first)
	r.start(context.Background(), json.RawMessage(`"dup"`), second.exitOnCancel)
	awaitStarted(t, second)
	if !r.cancelKey(`"dup"`) {
		t.Fatal("cancelKey must reach the latest worker under a duplicate id")
	}
	if !r.drain(context.Background(), 50*time.Millisecond, 2*time.Second) {
		t.Fatal("drain must cancel and join every in-flight worker, including one whose keyed entry a duplicate id overwrote")
	}
}

func TestInflightDuplicateIDReleaseKeepsLatestEntry(t *testing.T) {
	r := newInflightRequests()
	first, second := newFakeWorker(), newFakeWorker()
	r.start(context.Background(), json.RawMessage(`"dup"`), first.ignoreCancel)
	awaitStarted(t, first)
	r.start(context.Background(), json.RawMessage(`"dup"`), second.exitOnCancel)
	awaitStarted(t, second)
	close(first.released)
	live := 0
	for deadline := time.Now().Add(2 * time.Second); ; {
		r.mu.Lock()
		live = len(r.slots)
		r.mu.Unlock()
		if live == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if live != 1 {
		t.Fatalf("the finished worker must leave exactly one live slot, found %d", live)
	}
	if !r.cancelKey(`"dup"`) {
		t.Fatal("a finished worker's release must not delete the newer worker's keyed entry")
	}
	if !r.drain(context.Background(), 2*time.Second, 2*time.Second) {
		t.Fatal("drain must join the surviving duplicate")
	}
}

func TestInflightStartRefusedOnceDraining(t *testing.T) {
	r := newInflightRequests()
	stuck := newFakeWorker()
	r.start(context.Background(), json.RawMessage(`"stuck"`), stuck.ignoreCancel)
	awaitStarted(t, stuck)
	drained := make(chan bool, 1)
	go func() {
		drained <- r.drain(context.Background(), 50*time.Millisecond, 100*time.Millisecond)
	}()
	draining := false
	for deadline := time.Now().Add(2 * time.Second); ; {
		r.mu.Lock()
		draining = r.draining
		r.mu.Unlock()
		if draining || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !draining {
		t.Fatal("drain must mark the registry draining")
	}
	late := newFakeWorker()
	r.start(context.Background(), json.RawMessage(`"late"`), late.exitOnCancel)
	select {
	case <-late.started:
		t.Fatal("start during drain must not spawn the worker")
	default:
	}
	if r.cancelKey(`"late"`) {
		t.Fatal("a refused start must not register a keyed entry")
	}
	<-drained
	close(stuck.released)
	if !r.drain(context.Background(), 2*time.Second, 2*time.Second) {
		t.Fatal("after the stuck worker is released, a later drain must join it")
	}
}

func TestInflightNonCanonicalIDStaysDrainReachable(t *testing.T) {
	r := newInflightRequests()
	w := newFakeWorker()
	r.start(context.Background(), json.RawMessage(`null`), w.exitOnCancel)
	awaitStarted(t, w)
	if r.cancelKey(``) || r.cancelKey(`null`) {
		t.Fatal("an id that does not canonicalize must not be keyed")
	}
	if !r.drain(context.Background(), 50*time.Millisecond, 2*time.Second) {
		t.Fatal("a worker without a canonical key must still be drained by the cancel-all sweep")
	}
}

func TestDrainJoinsIdleRegistry(t *testing.T) {
	r := newInflightRequests()
	if !r.drain(context.Background(), 5*time.Second, 5*time.Second) {
		t.Fatal("drain on an idle registry must report a clean join")
	}
}

func TestDrainCancelsResponsiveWorkers(t *testing.T) {
	t.Run("stdin EOF", func(t *testing.T) {
		r := newInflightRequests()
		w := newFakeWorker()
		r.start(context.Background(), json.RawMessage(`"w"`), w.exitOnCancel)
		awaitStarted(t, w)
		if !r.drain(context.Background(), 50*time.Millisecond, 2*time.Second) {
			t.Fatal("drain after stdin EOF must cancel and join a context-aware worker")
		}
	})
	t.Run("signal", func(t *testing.T) {
		parent, cancelParent := context.WithCancel(context.Background())
		r := newInflightRequests()
		w := newFakeWorker()
		r.start(parent, json.RawMessage(`"w"`), w.exitOnCancel)
		awaitStarted(t, w)
		cancelParent()
		if !r.drain(context.Background(), 2*time.Second, 2*time.Second) {
			t.Fatal("a worker whose parent context a signal cancelled must join during the grace phase")
		}
	})
	t.Run("read error", func(t *testing.T) {
		r := newInflightRequests()
		gone, live := newFakeWorker(), newFakeWorker()
		r.start(context.Background(), json.RawMessage(`"gone"`), gone.immediately)
		r.start(context.Background(), json.RawMessage(`"live"`), live.exitOnCancel)
		awaitStarted(t, gone, live)
		if !r.drain(context.Background(), 50*time.Millisecond, 2*time.Second) {
			t.Fatal("drain after a read error must cancel and join the still-running worker")
		}
	})
}

func TestDrainBoundHoldsUnderUnresponsiveWorkers(t *testing.T) {
	r := newInflightRequests()
	stuckA, stuckB := newFakeWorker(), newFakeWorker()
	r.start(context.Background(), json.RawMessage(`"stuck-a"`), stuckA.ignoreCancel)
	r.start(context.Background(), json.RawMessage(`"stuck-b"`), stuckB.ignoreCancel)
	awaitStarted(t, stuckA, stuckB)
	r.cancelKey(`"stuck-a"`)
	start := time.Now()
	if r.drain(context.Background(), 50*time.Millisecond, 100*time.Millisecond) {
		t.Fatal("drain must report that unresponsive workers did not join")
	}
	if elapsed := time.Since(start); elapsed < 140*time.Millisecond {
		t.Fatalf("drain must honor the full grace window before cancelling: %v", elapsed)
	}
	close(stuckA.released)
	close(stuckB.released)
	if !r.drain(context.Background(), 2*time.Second, 2*time.Second) {
		t.Fatal("once the unresponsive workers are released, drain must join them")
	}
}

func TestDrainContextInterruptCutsTheWait(t *testing.T) {
	r := newInflightRequests()
	stuck := newFakeWorker()
	r.start(context.Background(), json.RawMessage(`"stuck"`), stuck.ignoreCancel)
	awaitStarted(t, stuck)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if r.drain(ctx, 5*time.Second, 5*time.Second) {
		t.Fatal("drain must not report a clean join when its context is cancelled mid-wait")
	}
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Fatal("a cancelled drain context must cut the grace wait instead of waiting it out")
	}
	close(stuck.released)
	if !r.drain(context.Background(), 2*time.Second, 2*time.Second) {
		t.Fatal("after the unresponsive worker is released, a fresh drain must join it")
	}
}

func TestInflightConcurrentStartsAndCancels(t *testing.T) {
	r := newInflightRequests()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				key := fmt.Sprintf(`"c-%d-%d"`, i, j)
				w := newFakeWorker()
				r.start(context.Background(), json.RawMessage(key), w.exitOnCancel)
				r.cancelKey(key)
			}
		}(i)
	}
	wg.Wait()
	if !r.drain(context.Background(), 2*time.Second, 2*time.Second) {
		t.Fatal("after every keyed cancel, drain must join every worker")
	}
}
