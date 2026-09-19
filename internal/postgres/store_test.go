package postgres

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/store"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	dsn := os.Getenv("DOLMEN_TEST_PG_DSN")
	if dsn == "" {
		if os.Getenv("DOLMEN_TEST_PG_REQUIRED") == "1" {
			t.Fatal("DOLMEN_TEST_PG_DSN is required in the PostgreSQL CI job")
		}
		t.Skip("set DOLMEN_TEST_PG_DSN to run PostgreSQL integration tests")
	}
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	cfg := Config{DSN: dsn, Catalog: "dolmen_test_" + hex.EncodeToString(id[:]), QueryRole: os.Getenv("DOLMEN_TEST_PG_QUERY_ROLE"), MaxConns: 4}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, cfg.DSN)
		if err != nil {
			t.Errorf("cleanup connect: %v", err)
			return
		}
		defer conn.Close(ctx)
		rows, err := conn.Query(ctx, "SELECT physical FROM "+ident(cfg.Catalog, "namespaces"))
		if err == nil {
			var names []string
			for rows.Next() {
				var name string
				if err := rows.Scan(&name); err != nil {
					t.Error(err)
					break
				}
				names = append(names, name)
			}
			rows.Close()
			for _, name := range names {
				if _, err := conn.Exec(ctx, "DROP SCHEMA IF EXISTS "+ident(name)+" CASCADE"); err != nil {
					t.Error(err)
				}
			}
		}
		if _, err := conn.Exec(ctx, "DROP SCHEMA IF EXISTS "+ident(cfg.Catalog)+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	return cfg
}

func openTest(t *testing.T, cfg Config) *Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestOpenRejectsInvalidConfig(t *testing.T) {
	for _, cfg := range []Config{
		{}, {DSN: "postgres://localhost/test", Catalog: "public"},
		{DSN: "postgres://localhost/test", Catalog: "pg_private"},
		{DSN: "postgres://localhost/test", Catalog: "information_schema"},
		{DSN: "postgres://localhost/test", Catalog: "x; DROP SCHEMA public"},
		{DSN: "postgres://localhost/test", Catalog: strings.Repeat("x", 64)},
		{DSN: "postgres://localhost/test", QueryRole: "query; SET ROLE postgres"},
		{DSN: "postgres://localhost/test", MaxConns: -1},
		{DSN: "postgres://user:secret-password@localhost:invalid/test"},
	} {
		s, err := Open(context.Background(), cfg)
		if s != nil || !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("invalid config: store=%v err=%v", s, err)
		}
		if strings.Contains(err.Error(), "secret-password") {
			t.Fatal("error exposes password")
		}
	}
}

func TestPostgresBootstrapConcurrentAndVersionGuard(t *testing.T) {
	cfg := testConfig(t)
	ctx := context.Background()
	const clients = 6
	var wg sync.WaitGroup
	results := make(chan *Store, clients)
	failures := make(chan error, clients)
	for range clients {
		wg.Go(func() {
			s, err := Open(ctx, cfg)
			if err != nil {
				failures <- err
			} else {
				results <- s
			}
		})
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	var s *Store
	for opened := range results {
		if s == nil {
			s = opened
		}
		t.Cleanup(func() { opened.Close() })
	}
	if s == nil {
		t.Fatal("no store opened")
	}
	if _, err := s.pool.Exec(ctx, "UPDATE "+s.relation("version")+" SET version = $1", catalogVersion+1); err != nil {
		t.Fatal(err)
	}
	newer, err := Open(ctx, cfg)
	if newer != nil || !errors.Is(err, store.ErrCatalogTooNew) {
		t.Fatalf("future catalog: %v", err)
	}
	if !strings.Contains(err.Error(), "use a compatible dolmen release") || strings.Contains(err.Error(), "connection settings") {
		t.Fatalf("future catalog remediation hidden: %v", err)
	}
	var version int
	if err := s.pool.QueryRow(ctx, "SELECT version FROM "+s.relation("version")).Scan(&version); err != nil || version != catalogVersion+1 {
		t.Fatalf("future catalog changed: %d %v", version, err)
	}
}

func TestPostgresNamespaceGenerationAndAtomicDDL(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	other := openTest(t, cfg)
	ctx := context.Background()
	name := strings.Repeat("a", 64) + "/" + strings.Repeat("b", 64) + "/leaf"
	if err := s.CreateNamespace(ctx, name, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	gen, err := other.NamespaceState(ctx, name, nil)
	if err != nil || gen == [16]byte{} {
		t.Fatalf("state: %x %v", gen, err)
	}
	if err := other.CreateNamespace(ctx, name, [16]byte{}); !errors.Is(err, store.ErrExists) {
		t.Fatalf("duplicate: %v", err)
	}
	if err := s.DropNamespace(ctx, name, gen); err != nil {
		t.Fatal(err)
	}
	if err := other.CreateNamespace(ctx, name, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	next, err := s.NamespaceState(ctx, name, nil)
	if err != nil || next == gen {
		t.Fatalf("generation reused: %v", err)
	}
	if err := s.DropNamespace(ctx, name, gen); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale generation dropped successor: %v", err)
	}
	if err := s.CreateNamespace(ctx, "parent", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	parent, _ := s.NamespaceState(ctx, "parent", nil)
	stale := parent
	stale[0] ^= 1
	if err := s.CreateNamespace(ctx, "parent/rejected", stale); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale parent: %v", err)
	}
	if err := s.CreateNamespace(ctx, "parent/child", parent); err != nil {
		t.Fatal(err)
	}
	if err := s.DropNamespace(ctx, "parent", parent); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("parent drop: %v", err)
	}
	var physical string
	if err := s.pool.QueryRow(ctx, "SELECT physical FROM "+s.relation("namespaces")+" WHERE name=$1", name).Scan(&physical); err != nil {
		t.Fatal(err)
	}
	if len(physical) > 63 {
		t.Fatalf("physical identifier too long: %s", physical)
	}
	var orphanCount int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM "+s.relation("namespaces")+" WHERE name='parent/rejected'").Scan(&orphanCount); err != nil || orphanCount != 0 {
		t.Fatalf("failed creation persisted: %d %v", orphanCount, err)
	}
	if err := s.write(ctx, name, next, func(tx pgx.Tx, n namespace) error {
		_, err := tx.Exec(ctx, "CREATE TABLE "+ident(n.physical, "payload")+"(v integer)")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DropNamespace(ctx, name, next); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)", physical).Scan(&exists); err != nil || exists {
		t.Fatalf("physical schema survived: %t %v", exists, err)
	}
}

func TestPostgresWriteRollbackAndCanceledLock(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	other := openTest(t, cfg)
	ctx := context.Background()
	if err := s.CreateNamespace(ctx, "events", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	abort := errors.New("abort")
	if err := s.write(ctx, "events", [16]byte{}, func(tx pgx.Tx, n namespace) error {
		if _, err := s.reserveChanges(ctx, tx, n, 7); err != nil {
			return err
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatal(err)
	}
	if err := s.write(ctx, "events", [16]byte{}, func(tx pgx.Tx, n namespace) error {
		r, err := s.reserveChanges(ctx, tx, n, 2)
		if err == nil && (r.First != 1 || r.Last != 2) {
			t.Errorf("rolled back allocation persisted: %+v", r)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateNamespace(ctx, "independent", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.write(ctx, "events", [16]byte{}, func(tx pgx.Tx, n namespace) error { close(locked); <-release; return nil })
	}()
	<-locked
	independentCtx, independentCancel := context.WithTimeout(ctx, time.Second)
	independentErr := other.write(independentCtx, "independent", [16]byte{}, func(tx pgx.Tx, n namespace) error {
		_, err := other.reserveChanges(independentCtx, tx, n, 1)
		return err
	})
	independentCancel()
	if independentErr != nil {
		close(release)
		<-done
		t.Fatalf("another namespace was blocked: %v", independentErr)
	}
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	err := other.write(cctx, "events", [16]byte{}, func(pgx.Tx, namespace) error { return errors.New("unexpected lock admission") })
	cancel()
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("lock cancellation: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := other.write(ctx, "events", [16]byte{}, func(tx pgx.Tx, n namespace) error { _, err := other.reserveChanges(ctx, tx, n, 1); return err }); err != nil {
		t.Fatalf("pool not reusable after cancel: %v", err)
	}
}

func TestPostgresCloseWaitsForAdmittedWrite(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	ctx := context.Background()
	if err := s.CreateNamespace(ctx, "events", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	admitted := make(chan struct{})
	release := make(chan struct{})
	written := make(chan error, 1)
	go func() {
		written <- s.write(ctx, "events", [16]byte{}, func(tx pgx.Tx, n namespace) error {
			close(admitted)
			<-release
			_, err := s.reserveChanges(ctx, tx, n, 1)
			return err
		})
	}()
	<-admitted
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		s.mu.Lock()
		closing := s.closed
		s.mu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("close did not start")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-closed:
		close(release)
		t.Fatalf("close returned before write finished: %v", err)
	default:
	}
	if _, err := s.ListNamespaces(ctx, "", nil); !errors.Is(err, store.ErrClosed) {
		t.Errorf("operation after close: %v", err)
	}
	close(release)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NamespaceState(ctx, "events", nil); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("closed store reopened: %v", err)
	}
}

func TestPostgresWorker(t *testing.T) {
	catalog := os.Getenv("DOLMEN_PG_WORKER_CATALOG")
	if catalog == "" {
		t.Skip("subprocess helper")
	}
	s, err := Open(context.Background(), Config{DSN: os.Getenv("DOLMEN_TEST_PG_DSN"), Catalog: catalog, MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for range 12 {
		err := s.write(ctx, "events", [16]byte{}, func(tx pgx.Tx, n namespace) error {
			r, err := s.reserveChanges(ctx, tx, n, 1)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "INSERT INTO "+ident(n.physical, "events")+"(position) VALUES($1)", r.Last); err != nil {
				return err
			}
			time.Sleep(3 * time.Millisecond)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestPostgresCommitOrderAcrossProcesses(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	ctx := context.Background()
	if err := s.CreateNamespace(ctx, "events", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	var physical string
	if err := s.write(ctx, "events", [16]byte{}, func(tx pgx.Tx, n namespace) error {
		physical = n.physical
		_, err := tx.Exec(ctx, "CREATE TABLE "+ident(n.physical, "events")+"(position bigint PRIMARY KEY)")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	runctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	failures := make(chan error, 2)
	for range 2 {
		cmd := exec.CommandContext(runctx, os.Args[0], "-test.run=^TestPostgresWorker$", "-test.count=1")
		cmd.Env = append(os.Environ(), "DOLMEN_PG_WORKER_CATALOG="+cfg.Catalog)
		var output bytes.Buffer
		cmd.Stdout = &output
		cmd.Stderr = &output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() {
			err := cmd.Wait()
			if err != nil {
				err = fmt.Errorf("worker: %w\n%s", err, output.String())
			}
			failures <- err
		}()
	}
	var cursor int64
	for cursor < 24 && runctx.Err() == nil {
		rows, err := s.pool.Query(runctx, "SELECT position FROM "+ident(physical, "events")+" WHERE position > $1 ORDER BY position", cursor)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var position int64
			if err := rows.Scan(&position); err != nil {
				t.Fatal(err)
			}
			if position != cursor+1 {
				t.Fatalf("uncommitted change skipped: cursor=%d next=%d", cursor, position)
			}
			cursor = position
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	for range 2 {
		if err := <-failures; err != nil {
			t.Error(err)
		}
	}
	if cursor != 24 {
		t.Fatalf("only observed %d of 24 commits", cursor)
	}
}

func TestPostgresOpenCancellationAndFailClosedBindings(t *testing.T) {
	cfg := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s, err := Open(ctx, cfg)
	if s != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled open: %v", err)
	}
	s = openTest(t, cfg)
	ctx = context.Background()
	if err := s.CreateNamespace(ctx, "private", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NamespaceState(ctx, "private", []store.AuthBinding{{Root: true}}); err == nil {
		t.Fatal("unimplemented authorization accepted")
	}
	if _, err := s.ListNamespaces(ctx, "", []store.AuthBinding{{Root: true}}); err == nil {
		t.Fatal("unimplemented authorization listed namespaces")
	}
}
