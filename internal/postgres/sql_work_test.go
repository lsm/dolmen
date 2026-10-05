package postgres

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
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

func TestCanceledCompilerRollsBackAndReleasesNamespace(t *testing.T) {
	cfg := testConfig(t)
	st := openTest(t, cfg)
	other := openTest(t, cfg)
	if err := st.CreateNamespace(t.Context(), "work", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	runner := newSQLWork(1)
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	completed := make(chan error, 1)
	go func() {
		completed <- st.write(ctx, "work", [16]byte{}, func(tx pgx.Tx, n namespace) error {
			if _, err := tx.Exec(ctx, "CREATE TABLE "+ident(n.physical, "cancel_probe")+" (n bigint)"); err != nil {
				return err
			}
			_, _, err := runner.run(ctx, func() (string, *sqlNames, error) {
				close(entered)
				<-release
				return "late", nil, nil
			})
			return err
		})
	}()
	select {
	case <-entered:
	case err := <-completed:
		t.Fatalf("transaction ended before compilation: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("compiler was not entered")
	}
	cancel()
	select {
	case err := <-completed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled transaction: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled compiler retained its transaction")
	}
	check, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	if err := other.write(check, "work", [16]byte{}, func(tx pgx.Tx, n namespace) error {
		var rolledBack bool
		if err := tx.QueryRow(check, "SELECT to_regclass($1) IS NULL", ident(n.physical, "cancel_probe")).Scan(&rolledBack); err != nil {
			return err
		}
		if !rolledBack {
			t.Error("canceled transaction committed its schema mutation")
		}
		return nil
	}); err != nil {
		t.Fatalf("namespace stayed locked after canceled compilation: %v", err)
	}
}

type completedWorkTx struct {
	pgx.Tx
	rollbacks int
}

func (tx *completedWorkTx) Rollback(ctx context.Context) error {
	tx.rollbacks++
	return pgx.ErrTxClosed
}
func (tx *completedWorkTx) Conn() *pgx.Conn {
	panic("completed transaction's connection may already have another owner")
}

func TestCanceledCleanupDoesNotTouchACompletedTransactionsConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tx := &completedWorkTx{}
	rollbackContext(ctx, tx)
	if tx.rollbacks != 1 {
		t.Fatalf("cleanup attempts: %d", tx.rollbacks)
	}
}
