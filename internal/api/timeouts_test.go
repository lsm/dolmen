package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestConfigureAppliesEveryConnectionBound(t *testing.T) {
	srv := &http.Server{}
	Timeouts{Read: time.Second, Write: 2 * time.Second, Idle: 3 * time.Second, MaxHeaderBytes: 8192}.Configure(srv)
	if srv.ReadHeaderTimeout != readHeaderTimeout || srv.ReadTimeout != time.Second || srv.WriteTimeout != 2*time.Second || srv.IdleTimeout != 3*time.Second || srv.MaxHeaderBytes != 8192 {
		t.Fatalf("got read-header %s, read %s, write %s, idle %s, header bytes %d", srv.ReadHeaderTimeout, srv.ReadTimeout, srv.WriteTimeout, srv.IdleTimeout, srv.MaxHeaderBytes)
	}

	Timeouts{Read: time.Second}.Configure(srv)
	if srv.IdleTimeout >= 0 {
		t.Fatalf("an idle bound of 0 must disable idling out, but net/http falls back to the read timeout for a zero IdleTimeout; got %s", srv.IdleTimeout)
	}
	if srv.WriteTimeout != 0 {
		t.Fatalf("a write bound of 0 must leave writes unbounded, got %s", srv.WriteTimeout)
	}
}

func TestEveryOperationButMigrateAnswersToTheOperationLimit(t *testing.T) {
	s := New(nil, nil, WithTimeouts(Timeouts{Op: time.Minute, Migrate: time.Hour}))
	for _, op := range OpNames() {
		limit, _ := s.opLimit(op)
		want := time.Minute
		switch op {
		case "migrate":
			want = time.Hour
		case "wait_for":
			want = time.Minute + maxWaitForTimeoutMS*time.Millisecond
		}
		if limit != want {
			t.Errorf("%s: limit %s, want %s", op, limit, want)
		}
	}
	if limit, _ := New(nil, nil, WithTimeouts(Timeouts{Migrate: time.Hour})).opLimit("query"); limit != 0 {
		t.Fatalf("an operation limit of 0 must leave operations unbounded, got %s", limit)
	}
	if limit, _ := New(nil, nil, WithTimeouts(Timeouts{Op: time.Minute})).opLimit("migrate"); limit != 0 {
		t.Fatalf("a migration limit of 0 must leave migrate unbounded whatever the operation limit, got %s", limit)
	}
}

func TestACallersOwnDeadlineIsNotBlamedOnTheOperationLimit(t *testing.T) {
	srv := New(seedWaitForTable(t), fakeEmb{}, WithTimeouts(Timeouts{Op: time.Hour}))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := srv.Dispatch(ctx, "wait_for", []byte(`{"namespace":"cancelns","table":"t","timeout_ms":20000}`))
	got := WrapError(err)
	if got.Code != ErrCodeTimeout {
		t.Fatalf("an expired deadline must classify as timeout, got %s: %v", got.Code, err)
	}
	if strings.Contains(got.Message, "-op-timeout") {
		t.Fatalf("the caller's own deadline expired, not the server's hour-long operation limit, yet the error blames the limit: %q", got.Message)
	}

	srv = New(seedWaitForTable(t), fakeEmb{}, WithTimeouts(Timeouts{Op: 50 * time.Millisecond}))
	_, err = srv.Dispatch(context.Background(), "wait_for", []byte(`{"namespace":"cancelns","table":"t","timeout_ms":0}`))
	if err != nil {
		t.Fatalf("an immediate wait finishes inside any limit: %v", err)
	}
}
