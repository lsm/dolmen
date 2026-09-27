package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
	"modernc.org/sqlite"
)

type prepareCounter struct {
	inner  driver.Driver
	match  func(string) bool
	counts map[string]*atomic.Int64
	seen   atomic.Bool
}

type prepareCountingConn struct {
	driver.Conn
	d *prepareCounter
}

type prepareCountingStmt struct {
	driver.Stmt
	d *prepareCounter
}

func (d *prepareCounter) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &prepareCountingConn{Conn: conn, d: d}, nil
}

func (d *prepareCounter) count(query string) {
	if !d.match(query) {
		return
	}
	for key, n := range d.counts {
		if strings.Contains(query, key) {
			n.Add(1)
		}
	}
	d.seen.Store(true)
}

func (c *prepareCountingConn) Prepare(query string) (driver.Stmt, error) {
	c.d.count(query)
	st, err := c.Conn.Prepare(query)
	if err != nil {
		return nil, err
	}
	return &prepareCountingStmt{Stmt: st, d: c.d}, nil
}

func (c *prepareCountingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	c.d.count(query)
	if p, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, query)
	}
	return c.Conn.Prepare(query)
}

func (c *prepareCountingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c *prepareCountingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *prepareCountingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if b, ok := c.Conn.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, opts)
	}
	return c.Conn.Begin()
}

func (c *prepareCountingConn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *prepareCountingConn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *prepareCountingConn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

func (c *prepareCountingConn) Close() error { return c.Conn.Close() }

func (s *prepareCountingStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.Stmt.Exec(args)
}

func openStoreCountingPrepares(t *testing.T, rows int) (*Store, *atomic.Int64) {
	t.Helper()
	stamp := &atomic.Int64{}
	driverNamesMu.Lock()
	driverNames++
	name := fmt.Sprintf("dolmen-prepare-%d", driverNames)
	driverNamesMu.Unlock()
	d := &prepareCounter{
		inner:  &sqlite.Driver{},
		match:  func(query string) bool { return strings.Contains(query, `SET "_embedding" = ? WHERE id = ?`) },
		counts: map[string]*atomic.Int64{`SET "_embedding" = ? WHERE id = ?`: stamp},
	}
	sql.Register(name, d)
	st, err := Open(t.TempDir(), withOpenDB(func(dsn string) (*sql.DB, error) { return sql.Open(name, dsn) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	seedBackfillTable(t, st, "prep", "docs", rows)
	if _, err := st.Migrate(context.Background(), "prep", "docs", []schema.Change{
		{Op: schema.OpSetVectorize, Name: "body", Value: boolPtr(true)},
	}, (&backfillRecorder{}).embedder("test"), Incarnation{Version: 1}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st, stamp
}

func TestTheActivationPreparesItsPerRowStampOnce(t *testing.T) {
	const rows = 300
	st, stamp := openStoreCountingPrepares(t, rows)
	if live := liveEmbeddings(t, st, "prep", "docs"); len(live) != rows {
		t.Fatalf("%d rows carry a vector, want %d", len(live), rows)
	}
	got := stamp.Load()
	if got == 0 {
		t.Fatalf("the per-row stamp never reached the driver as a prepared statement over %d rows, so it went through the driver's per-call exec path, which compiles the statement again for every row", rows)
	}
	if got > 1 {
		t.Fatalf("the per-row stamp was prepared %d times over %d rows, want 1: it is the one statement the activation walks the table with", got, rows)
	}
}

func TestTheConstantActivationIsPreparedOnce(t *testing.T) {
	stamp := &atomic.Int64{}
	driverNamesMu.Lock()
	driverNames++
	name := fmt.Sprintf("dolmen-prepare-const-%d", driverNames)
	driverNamesMu.Unlock()
	d := &prepareCounter{
		inner: &sqlite.Driver{},
		match: func(query string) bool {
			return strings.Contains(query, `SET "_embedding" = ? WHERE`) && !strings.Contains(query, "WHERE id = ?")
		},
		counts: map[string]*atomic.Int64{`SET "_embedding" = ? WHERE`: stamp},
	}
	sql.Register(name, d)
	st, err := Open(t.TempDir(), withOpenDB(func(dsn string) (*sql.DB, error) { return sql.Open(name, dsn) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	if err := st.CreateNamespace(ctx, "konst", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTable(ctx, "konst", "docs", []schema.Field{{Name: "body", Type: schema.Text}}, TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if _, err := st.Insert(ctx, "konst", "docs", []map[string]any{{"body": fmt.Sprintf("row %d", i)}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
			t.Fatal(err)
		}
	}
	before := stamp.Load()
	if _, err := st.Migrate(ctx, "konst", "docs", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "tag", Type: schema.String}, Default: "one text for every row"},
		{Op: schema.OpSetVectorize, Name: "tag", Value: boolPtr(true)},
	}, (&backfillRecorder{}).embedder("test"), Incarnation{Version: 1}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if got := stamp.Load() - before; got != 1 {
		t.Fatalf("the constant stamp was prepared %d times, want 1", got)
	}
	if live := liveEmbeddings(t, st, "konst", "docs"); len(live) != 50 {
		t.Fatalf("%d rows carry a vector, want all 50", len(live))
	}
}
