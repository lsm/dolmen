package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	sqlite "modernc.org/sqlite"

	"github.com/lsm/dolmen/internal/schema"
)

var (
	afterQueryNamesMu sync.Mutex
	afterQueryNames   int
)

type afterQueryDriver struct {
	inner driver.Driver
	match func(query string) bool
	after func()
	fired atomic.Bool
}

func (d *afterQueryDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &afterQueryConn{Conn: conn, d: d}, nil
}

func (d *afterQueryDriver) fire(query string) {
	if d.fired.Load() || !d.match(query) {
		return
	}
	if !d.fired.CompareAndSwap(false, true) {
		return
	}
	d.after()
}

type afterQueryConn struct {
	driver.Conn
	d *afterQueryDriver
}

func (c *afterQueryConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	return &afterQueryRows{Rows: rows, query: query, d: c.d}, nil
}

func (c *afterQueryConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *afterQueryConn) Prepare(query string) (driver.Stmt, error) {
	return c.Conn.Prepare(query)
}

func (c *afterQueryConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if p, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, query)
	}
	return c.Conn.Prepare(query)
}

func (c *afterQueryConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if b, ok := c.Conn.(driver.ConnBeginTx); ok {
		return b.BeginTx(ctx, opts)
	}
	return c.Conn.Begin()
}

func (c *afterQueryConn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c *afterQueryConn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c *afterQueryConn) IsValid() bool {
	if v, ok := c.Conn.(driver.Validator); ok {
		return v.IsValid()
	}
	return true
}

type afterQueryRows struct {
	driver.Rows
	query string
	d     *afterQueryDriver
}

func (r *afterQueryRows) Close() error {
	err := r.Rows.Close()
	r.d.fire(r.query)
	return err
}

func openStoreBehindAfterQuery(t *testing.T, match func(string) bool, after func()) *Store {
	t.Helper()
	afterQueryNamesMu.Lock()
	afterQueryNames++
	name := fmt.Sprintf("dolmen-after-query-%d", afterQueryNames)
	afterQueryNamesMu.Unlock()
	d := &afterQueryDriver{inner: &sqlite.Driver{}, match: match, after: after}
	sql.Register(name, d)
	st, err := Open(t.TempDir(), withOpenDB(func(dsn string) (*sql.DB, error) { return sql.Open(name, dsn) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func seedTaggedDocs(t *testing.T, st *Store, ns, table string, emb Embedder, tags ...string) {
	t.Helper()
	ctx := context.Background()
	if err := st.CreateNamespace(ctx, ns, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTable(ctx, ns, table, []schema.Field{
		{Name: "body", Type: schema.Text},
		{Name: "tag", Type: schema.Text},
	}, TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	for _, tag := range tags {
		if _, err := st.Insert(ctx, ns, table, []map[string]any{
			{"tag": tag, "body": "the body of the row tagged " + tag},
		}, WriteOpts{}, emb, nil, Incarnation{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Migrate(ctx, ns, table, vectorizeBody(), emb, Incarnation{Version: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestAWriteThatMatchesOnlyAfterThePreReadEmbedsInsideTheWriter(t *testing.T) {
	const ns, table = "win", "docs"
	const filter = "tag = 'late'"
	const newText = "the text the caller set while the writer was being taken"
	const interleaved = "the row written into the gap between the two reads"

	cases := []struct {
		name string
		run  func(ctx context.Context, st *Store, emb Embedder) (int64, error)
	}{
		{
			name: "update",
			run: func(ctx context.Context, st *Store, emb Embedder) (int64, error) {
				res, err := st.Update(ctx, ns, table, filter, nil, map[string]any{"body": newText}, emb, nil, Incarnation{})
				return res.Updated, err
			},
		},
		{
			name: "upsert",
			run: func(ctx context.Context, st *Store, emb Embedder) (int64, error) {
				res, err := st.Upsert(ctx, ns, table, filter, nil, map[string]any{"body": newText}, WriteOpts{}, emb, nil, Incarnation{})
				return res.Updated, err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			rec := &backfillRecorder{}
			emb := rec.embedder("fake-space")

			var st *Store
			var windowErr error
			var matchedID int64
			st = openStoreBehindAfterQuery(t,
				func(query string) bool {
					return strings.HasPrefix(query, `SELECT count(*) FROM "`+table+`" WHERE `+filter)
				},
				func() {
					res, err := st.Insert(ctx, ns, table, []map[string]any{
						{"tag": "late", "body": interleaved},
					}, WriteOpts{}, emb, nil, Incarnation{})
					if err != nil {
						windowErr = err
						return
					}
					if len(res.Ids) != 1 {
						windowErr = fmt.Errorf("the row written into the window has ids %v, want exactly one", res.Ids)
						return
					}
					matchedID = res.Ids[0]
				})

			seedTaggedDocs(t, st, ns, table, emb, "early")

			updated, err := tc.run(ctx, st, emb)
			if err != nil {
				t.Fatalf("%s failed: %v", tc.name, err)
			}
			if windowErr != nil {
				t.Fatalf("could not write into the gap between the pre-read and the writer, so the window was not exercised at all: %v", windowErr)
			}
			if matchedID == 0 {
				t.Fatalf("no row was written into the window, so the pre-read and the writer saw the same rows")
			}
			if updated != 1 {
				t.Fatalf("%s touched %d rows, want the one row that matched only after the pre-read had reported none", tc.name, updated)
			}
			if live := liveEmbeddings(t, st, ns, table)[matchedID]; len(live) == 0 {
				t.Fatalf("row %d matched the filter only after the pre-read had already reported none, and %s left it with no vector at all: the new text %q is in the row and nothing can find it by meaning. The pre-read count decides whether to embed before the writer is taken and is not a promise about the count the writer finds, so the writer has to ask for the vector its own count implies",
					matchedID, tc.name, newText)
			}
			expectVectorOf(t, st, ns, table, matchedID, newText)
		})
	}
}
