package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	sqlite "modernc.org/sqlite"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
)

var errDriverInterrupt = errors.New("interrupted (9)")

type interruptDriver struct {
	inner  driver.Driver
	match  func(query string) bool
	cancel context.CancelFunc
	fired  atomic.Bool
	seen   atomic.Pointer[string]
	allMu  sync.Mutex
	all    []string
}

func (d *interruptDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &interruptConn{Conn: conn, d: d}, nil
}

func (d *interruptDriver) fire(query string) bool {
	if d.fired.Load() || !d.match(query) {
		return false
	}
	if !d.fired.CompareAndSwap(false, true) {
		return false
	}
	held := query
	d.seen.Store(&held)
	d.cancel()
	return true
}

type interruptConn struct {
	driver.Conn
	d *interruptDriver
}

func (c *interruptConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.d.allMu.Lock()
	c.d.all = append(c.d.all, query)
	c.d.allMu.Unlock()
	if c.d.fire(query) {
		return nil, errDriverInterrupt
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c *interruptConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *interruptConn) Prepare(query string) (driver.Stmt, error) {
	return c.Conn.Prepare(query)
}

func (c *interruptConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if p, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, query)
	}
	return c.Conn.Prepare(query)
}

func (c *interruptConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if b, ok := c.Conn.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, opts)
	}
	return c.Conn.Begin()
}

func (c *interruptConn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *interruptConn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *interruptConn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

var (
	driverNamesMu sync.Mutex
	driverNames   int
)

func openStoreBehindInterruptDriver(t *testing.T, match func(string) bool, cancel context.CancelFunc) (*Store, *interruptDriver) {
	t.Helper()
	driverNamesMu.Lock()
	driverNames++
	name := fmt.Sprintf("dolmen-interrupt-%d", driverNames)
	driverNamesMu.Unlock()
	d := &interruptDriver{inner: &sqlite.Driver{}, match: match, cancel: cancel}
	sql.Register(name, d)
	t.Cleanup(func() { d.fired.Store(true) })
	st, err := Open(t.TempDir(), withOpenDB(func(dsn string) (*sql.DB, error) { return sql.Open(name, dsn) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, d
}

func matchProbe(query string) bool {
	return strings.Contains(query, "interrupt_probe")
}

func fireOn(t *testing.T, d *interruptDriver) {
	t.Helper()
	d.allMu.Lock()
	for _, q := range d.all {
		t.Logf("saw: %s", q)
	}
	d.allMu.Unlock()
	if !d.fired.Load() {
		t.Fatal("the wrapper never interrupted a statement, so this proves nothing")
	}
	if seen := d.seen.Load(); seen != nil {
		t.Logf("interrupted: %s", *seen)
	}
}

func TestADriverThatReportsItsOwnInterruptReachesTheCallerIntact(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	st, d := openStoreBehindInterruptDriver(t, matchProbe, cancel)
	if err := st.CreateNamespace(ctx, "iv", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTable(ctx, "iv", "interrupt_probe", []schema.Field{{Name: "body", Type: schema.Text}}, TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Insert(ctx, "iv", "interrupt_probe", []map[string]any{{"body": "one"}}, WriteOpts{}, testEmbed, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	_, err := st.GetRows(ctx, "iv", "interrupt_probe", []int64{1}, nil, Incarnation{})
	fireOn(t, d)
	if !errors.Is(err, errDriverInterrupt) {
		t.Fatalf("read_rows returned %v, want the driver's own error: the store must hand an engine error up unchanged for the boundary to classify", err)
	}
	if got := SpanErrorType(err); got != string(derr.Internal) {
		t.Fatalf("the store classified the interrupt as %q, want internal_error: nothing below the boundary can tell a cancelled caller from a fault", got)
	}
	if ctx.Err() == nil {
		t.Fatal("the caller's context is still live, so this is not a cancellation at all")
	}
	if _, err := st.GetRows(context.Background(), "iv", "interrupt_probe", []int64{1}, nil, Incarnation{}); err != nil {
		t.Fatalf("the read connection must be usable after the interrupt: %v", err)
	}
}
