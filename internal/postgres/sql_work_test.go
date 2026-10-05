package postgres

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSQLCompilerCancellationReleasesCallerAndBoundsWorkers(t *testing.T) {
	runner := newSQLWork(1)
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, _, err := runner.run(ctx, func() (string, *sqlNames, error) {
			close(started)
			<-release
			close(finished)
			return "late", nil, nil
		})
		result <- err
	}()
	<-started
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled compiler: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("compiler held its caller after cancellation")
	}
	waiting, stop := context.WithCancel(context.Background())
	stop()
	if _, _, err := runner.run(waiting, func() (string, *sqlNames, error) {
		t.Error("another compiler started while the worker was still occupied")
		return "", nil, nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("compiler admission: %v", err)
	}
	select {
	case <-finished:
		t.Fatal("the foreign parser was not blocked, so cancellation was not exercised")
	default:
	}
}

func TestSQLCompilerWalkStopsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := sqlCompiler{ctx: ctx}
	if err := c.walk(nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("compiler traversed after cancellation: %v", err)
	}
}
