package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/api"
	"github.com/lsm/dolmen/internal/store"
)

func servingWith(t *testing.T, h http.HandlerFunc) (*http.Server, *api.Server, string) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	apiSrv := api.New(st, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: apiSrv.Track(h)}
	go srv.Serve(ln)
	return srv, apiSrv, "http://" + ln.Addr().String()
}

func TestShutdownWhenIdle(t *testing.T) {
	srv, apiSrv, _ := servingWith(t, func(w http.ResponseWriter, r *http.Request) {})
	closed := false
	if err := shutdown(srv, apiSrv, time.Second, func() error { closed = true; return nil }); err != nil || !closed {
		t.Fatalf("idle shutdown: err %v, store closed %v", err, closed)
	}
}

func TestShutdownLetsARunningRequestFinish(t *testing.T) {
	started := make(chan struct{})
	srv, apiSrv, url := servingWith(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		w.Write([]byte("done"))
	})
	got := make(chan error, 1)
	go func() {
		res, err := http.Get(url)
		if err == nil {
			res.Body.Close()
			if res.StatusCode != 200 {
				err = errors.New(res.Status)
			}
		}
		got <- err
	}()
	<-started
	if err := shutdown(srv, apiSrv, 10*time.Second); err != nil {
		t.Fatalf("shutdown with a request inside the grace period: %v", err)
	}
	if err := <-got; err != nil {
		t.Fatalf("the running request must complete: %v", err)
	}
}

func TestShutdownCancelsWorkPastTheGrace(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	srv, apiSrv, url := servingWith(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
	})
	go http.Get(url)
	<-started
	err := shutdown(srv, apiSrv, 100*time.Millisecond)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a grace that expires must be reported, got %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("work past the grace must be cancelled")
	}
}

func TestShutdownReportsACloseFailure(t *testing.T) {
	srv, apiSrv, _ := servingWith(t, func(w http.ResponseWriter, r *http.Request) {})
	err := shutdown(srv, apiSrv, time.Second, func() error { return errors.New("checkpoint failed") }, func() error { return nil })
	if err == nil || !strings.Contains(err.Error(), "checkpoint failed") {
		t.Fatalf("a close error must reach the exit status, got %v", err)
	}
}
