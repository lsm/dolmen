package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/schema"
)

func insertNotes(t *testing.T, st *Store, n int) InsertResult {
	t.Helper()
	recs := make([]map[string]any, n)
	for i := range recs {
		recs[i] = map[string]any{"title": string(rune('a' + i%26)), "score": i + 1}
	}
	res, err := st.Insert(context.Background(), "test", "notes", recs, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("insert %d notes: %v", n, err)
	}
	return res
}

func kindsOf(records []ChangeRecord) []ChangeKind {
	out := make([]ChangeKind, len(records))
	for i, r := range records {
		out[i] = r.Kind
	}
	return out
}

func rowIDsOf(records []ChangeRecord) []int64 {
	out := make([]int64, len(records))
	for i, r := range records {
		out[i] = r.RowID
	}
	return out
}

func TestChangesSinceBareStartIsHead(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	insertNotes(t, st, 3)

	records, next, err := st.ChangesSince(ctx, "test", "", "", [16]byte{}, nil, Incarnation{}, Page{})
	if err != nil {
		t.Fatalf("bare start: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("bare start replayed %d records, want 0 — the head is the boundary, never the backlog", len(records))
	}
	if next == "" {
		t.Fatalf("bare start returned no next_cursor — the response must carry the head cursor")
	}

	insertNotes(t, st, 2)
	records, _, err = st.ChangesSince(ctx, "test", "", next, [16]byte{}, nil, Incarnation{}, Page{})
	if err != nil {
		t.Fatalf("resume head cursor: %v", err)
	}
	if got := rowIDsOf(records); len(got) != 2 || got[0] != 4 || got[1] != 5 {
		t.Fatalf("resume after head delivered rows %v, want the two new rows [4 5]", got)
	}
	if k := kindsOf(records); len(k) != 2 || k[0] != ChangeInsert || k[1] != ChangeInsert {
		t.Fatalf("resume kinds = %v, want insert insert", k)
	}
}

func TestChangesSinceBeginReplaysRetainedHistory(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	nsGen, err := st.NamespaceState(ctx, "test", nil)
	if err != nil {
		t.Fatalf("namespace state: %v", err)
	}
	first := insertNotes(t, st, 2)
	if _, err := st.Update(ctx, "test", "notes", "id = ?", []any{first.Ids[0]}, map[string]any{"score": 10},
		Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := st.Delete(ctx, "test", "notes", "id = ?", []any{first.Ids[0]}, DeleteOpts{Confirm: true},
		nil, Incarnation{}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	records, next, err := st.ChangesSince(ctx, "test", "", CursorBegin, [16]byte{}, nil, Incarnation{}, Page{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if len(records) != 4 {
		t.Fatalf("begin delivered %d records, want 4 (two inserts, one update, one delete)", len(records))
	}
	want := []ChangeKind{ChangeInsert, ChangeInsert, ChangeUpdate, ChangeDelete}
	for i, k := range kindsOf(records) {
		if k != want[i] {
			t.Fatalf("kind order = %v, want %v", kindsOf(records), want)
		}
	}
	for i, r := range records {
		if r.Table != "notes" || r.Cursor == "" {
			t.Fatalf("record %d = %+v, want table notes and a minted cursor", i, r)
		}
		if r.Owner != "" {
			t.Fatalf("record %d owner = %q, want the unstamped empty label (9c lands stamping)", i, r.Owner)
		}
		if r.Lifetime.Table != "notes" || r.Lifetime.DropGen != 0 || r.Lifetime.NsGen != nsGen {
			t.Fatalf("record %d lifetime = %+v, want (notes, 0, the namespace's id)", i, r.Lifetime)
		}
	}

	for i, r := range records {
		after, _, err := st.ChangesSince(ctx, "test", "", r.Cursor, [16]byte{}, nil, Incarnation{}, Page{})
		if err != nil {
			t.Fatalf("resume record %d cursor: %v", i, err)
		}
		if len(after) != len(records)-i-1 {
			t.Fatalf("resume from record %d delivered %d records, want %d", i, len(after), len(records)-i-1)
		}
		for j := range after {
			if after[j].RowID != records[i+1+j].RowID || after[j].Kind != records[i+1+j].Kind {
				t.Fatalf("resume from record %d diverged at %d: %+v, want %+v", i, j, after[j], records[i+1+j])
			}
		}
	}
	if next == "" {
		t.Fatalf("begin returned no next_cursor")
	}

	drain, _, err := st.ChangesSince(ctx, "test", "", next, [16]byte{}, nil, Incarnation{}, Page{})
	if err != nil {
		t.Fatalf("resume next_cursor: %v", err)
	}
	if len(drain) != 0 {
		t.Fatalf("resume next_cursor delivered %d records, want 0 (the log is drained)", len(drain))
	}
}

func TestChangesSinceGapFreePaging(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	const total = 25
	insertNotes(t, st, total)

	var got []int64
	cursor := CursorBegin
	for {
		records, next, err := st.ChangesSince(ctx, "test", "", cursor, [16]byte{}, nil, Incarnation{}, Page{Limit: 4})
		if err != nil {
			t.Fatalf("page from %q: %v", cursor, err)
		}
		got = append(got, rowIDsOf(records)...)
		if len(records) < 4 {
			if next == "" {
				t.Fatalf("page returned no next_cursor")
			}
			break
		}
		cursor = next
	}
	if len(got) != total {
		t.Fatalf("paging delivered %d row ids, want %d", len(got), total)
	}
	for i, id := range got {
		if id != int64(i+1) {
			t.Fatalf("paged row ids = %v, want 1..%d in order", got, total)
		}
	}
}

func TestChangesSincePageLimitContract(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	insertNotes(t, st, 105)

	records, _, err := st.ChangesSince(ctx, "test", "", CursorBegin, [16]byte{}, nil, Incarnation{}, Page{})
	if err != nil {
		t.Fatalf("default limit: %v", err)
	}
	if len(records) != DefaultChangesPageLimit {
		t.Fatalf("default page = %d records, want %d", len(records), DefaultChangesPageLimit)
	}

	records, _, err = st.ChangesSince(ctx, "test", "", CursorBegin, [16]byte{}, nil, Incarnation{}, Page{Limit: -3})
	if err != nil {
		t.Fatalf("negative limit: %v", err)
	}
	if len(records) != DefaultChangesPageLimit {
		t.Fatalf("negative limit page = %d records, want the default %d", len(records), DefaultChangesPageLimit)
	}

	for i := 0; i < 9; i++ {
		insertNotes(t, st, 100)
	}
	insertNotes(t, st, 5)
	records, _, err = st.ChangesSince(ctx, "test", "", CursorBegin, [16]byte{}, nil, Incarnation{}, Page{Limit: 5000})
	if err != nil {
		t.Fatalf("over-max limit: %v", err)
	}
	if len(records) != MaxChangesPageLimit {
		t.Fatalf("over-max page = %d records, want the clamp at %d", len(records), MaxChangesPageLimit)
	}
}

func TestChangesSinceTableFeedCurrentLifetime(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	insertNotes(t, st, 2)

	_, preDrop, err := st.ChangesSince(ctx, "test", "notes", "", [16]byte{}, nil, Incarnation{}, Page{})
	if err != nil {
		t.Fatalf("table bare start: %v", err)
	}

	if err := st.DropTable(ctx, "test", "notes", Incarnation{}); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "notes", noteFields(), TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("recreate table: %v", err)
	}
	insertNotes(t, st, 1)

	records, _, err := st.ChangesSince(ctx, "test", "notes", CursorBegin, [16]byte{}, nil, Incarnation{}, Page{})
	if err != nil {
		t.Fatalf("begin on successor feed: %v", err)
	}
	if got := rowIDsOf(records); len(got) != 1 || got[0] != 1 {
		t.Fatalf("table feed after recreate = %v, want only the successor's row [1] (ids restart with the new table)", got)
	}

	records, _, err = st.ChangesSince(ctx, "test", "notes", preDrop, [16]byte{}, nil, Incarnation{}, Page{})
	if err != nil {
		t.Fatalf("resume pre-drop cursor: %v", err)
	}
	if got := rowIDsOf(records); len(got) != 1 || got[0] != 1 {
		t.Fatalf("pre-drop cursor on successor feed = %v, want only the successor's row [1]", got)
	}

	records, _, err = st.ChangesSince(ctx, "test", "", CursorBegin, [16]byte{}, nil, Incarnation{}, Page{})
	if err != nil {
		t.Fatalf("namespace begin: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("namespace feed = %d records, want 3 across both lifetimes", len(records))
	}

	if _, _, err := st.ChangesSince(ctx, "test", "missing", "", [16]byte{}, nil, Incarnation{}, Page{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("changes_since on a missing table: err = %v, want ErrNotFound", err)
	}
}

func TestChangesSinceFeedBindingAndNamespaceGuard(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "tasks", []schema.Field{{Name: "title", Type: schema.String}}, TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("create tasks: %v", err)
	}
	insertNotes(t, st, 1)
	_, nsCursor, err := st.ChangesSince(ctx, "test", "", CursorBegin, [16]byte{}, nil, Incarnation{}, Page{})
	if err != nil {
		t.Fatalf("namespace begin: %v", err)
	}
	if _, _, err := st.ChangesSince(ctx, "test", "notes", nsCursor, [16]byte{}, nil, Incarnation{}, Page{}); !errors.Is(err, ErrCursorCrossFeed) {
		t.Fatalf("namespace token on a table feed: err = %v, want ErrCursorCrossFeed", err)
	}

	_, notesCursor, err := st.ChangesSince(ctx, "test", "notes", "", [16]byte{}, nil, Incarnation{}, Page{})
	if err != nil {
		t.Fatalf("notes head: %v", err)
	}
	if _, _, err := st.ChangesSince(ctx, "test", "tasks", notesCursor, [16]byte{}, nil, Incarnation{}, Page{}); !errors.Is(err, ErrCursorCrossFeed) {
		t.Fatalf("notes token on the tasks feed: err = %v, want ErrCursorCrossFeed", err)
	}

	if _, _, err := st.ChangesSince(ctx, "absent", "", "", [16]byte{}, nil, Incarnation{}, Page{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("changes_since on an absent namespace: err = %v, want ErrNotFound", err)
	}
}

func TestChangesSinceReopenReplay(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	dir := st.dir
	insertNotes(t, st, 3)
	_, next, err := st.ChangesSince(ctx, "test", "", CursorBegin, [16]byte{}, nil, Incarnation{}, Page{Limit: 2})
	if err != nil {
		t.Fatalf("begin page: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { st2.Close() })
	records, _, err := st2.ChangesSince(ctx, "test", "", next, [16]byte{}, nil, Incarnation{}, Page{})
	if err != nil {
		t.Fatalf("resume after reopen: %v", err)
	}
	if got := rowIDsOf(records); len(got) != 1 || got[0] != 3 {
		t.Fatalf("resume after reopen = %v, want [3] — page 2 of the pre-restart chain", got)
	}
}

func TestChangesSinceBeginHonorsRetention(t *testing.T) {
	const day = 24 * time.Hour
	cases := []struct {
		name      string
		retention time.Duration
		mints     []time.Duration
		wantRow   int64
	}{
		{"retention skips pre-window records", 7 * day, []time.Duration{-30 * day, -1 * day}, 2},
		{"retention 0 replays from the oldest record", 0, []time.Duration{-30 * day, -1 * day}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, err := Open(t.TempDir(), WithChangeRetention(tc.retention))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			t.Cleanup(func() { st.Close() })
			mustNS(t, legacy(st), "test")
			if _, err := st.CreateTable(context.Background(), "test", "notes", noteFields(), TableOpts{}, [16]byte{}); err != nil {
				t.Fatalf("create table: %v", err)
			}
			n, err := st.ns("test")
			if err != nil {
				t.Fatalf("ns: %v", err)
			}
			seedChanges(t, n, time.Now(), tc.mints)

			records, _, err := st.ChangesSince(context.Background(), "test", "", CursorBegin, [16]byte{}, nil, Incarnation{}, Page{})
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			if len(records) == 0 || records[0].RowID != tc.wantRow {
				t.Fatalf("first delivered record = %+v, want row_id %d (retention %v)", records, tc.wantRow, tc.retention)
			}
		})
	}
}

func TestChangesSinceCursorExpiry(t *testing.T) {
	st, err := Open(t.TempDir(), WithChangeRetention(40*time.Millisecond))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	mustNS(t, legacy(st), "test")
	if _, err := st.CreateTable(context.Background(), "test", "notes", noteFields(), TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	ctx := context.Background()
	insertNotes(t, st, 1)
	_, next, err := st.ChangesSince(ctx, "test", "", CursorBegin, [16]byte{}, nil, Incarnation{}, Page{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	time.Sleep(250 * time.Millisecond)
	if _, _, err := st.ChangesSince(ctx, "test", "", next, [16]byte{}, nil, Incarnation{}, Page{}); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("resume past the deadline: err = %v, want ErrCursorExpired", err)
	}

	if _, _, err := st.ChangesSince(ctx, "test", "", "deadbeefdeadbeefdeadbeefdeadbeef", [16]byte{}, nil, Incarnation{}, Page{}); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("unknown token: err = %v, want ErrCursorExpired", err)
	}
}

func TestChangesSinceZeroRetentionNeverExpires(t *testing.T) {
	st, err := Open(t.TempDir(), WithChangeRetention(0))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	mustNS(t, legacy(st), "test")
	if _, err := st.CreateTable(context.Background(), "test", "notes", noteFields(), TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	ctx := context.Background()
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	seedChanges(t, n, time.Now(), []time.Duration{-90 * 24 * time.Hour, -time.Hour})

	records, _, err := st.ChangesSince(ctx, "test", "", CursorBegin, [16]byte{}, nil, Incarnation{}, Page{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("begin with retention 0 = %d records, want both retained records", len(records))
	}
}

func TestChangesSincePrunesWithRetention(t *testing.T) {
	st, err := Open(t.TempDir(), WithChangeRetention(24*time.Hour))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	mustNS(t, legacy(st), "test")
	if _, err := st.CreateTable(context.Background(), "test", "notes", noteFields(), TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	ctx := context.Background()
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}

	now := time.Now()
	seedChanges(t, n, now, []time.Duration{-100 * 24 * time.Hour, -99 * 24 * time.Hour, -time.Hour})

	if _, _, err := st.ChangesSince(ctx, "test", "", CursorBegin, [16]byte{}, nil, Incarnation{}, Page{}); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if got := changeSeqs(t, n); !eqSeqs(got, []int64{3}) {
		t.Fatalf("records after the read = %v, want [3]: the two past-hold records are unreachable and pruned", got)
	}
}

func TestChangesLogLookupsIndexed(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	nsGen, err := readNSGen(ctx, n.ro)
	if err != nil {
		t.Fatalf("read nsgen: %v", err)
	}
	cases := []struct {
		name  string
		query string
		args  []any
		want  string
	}{
		{
			"table feed page read",
			`SELECT seq, table_name, row_id, kind, owner, nsgen, drop_gen FROM _dolmen_changes
			 WHERE seq > ? AND table_name = ? AND drop_gen = ? AND nsgen = ? ORDER BY seq LIMIT ?`,
			[]any{int64(0), "notes", int64(0), nsGen[:], 100},
			"_dolmen_changes_table_feed",
		},
		{
			"token deadline prune",
			`DELETE FROM _dolmen_cursor_tokens WHERE issued_at < ?`,
			[]any{int64(0)},
			"_dolmen_cursor_tokens_issued_at",
		},
		{
			"token cap prune",
			`DELETE FROM _dolmen_cursor_tokens WHERE chain_start < ?`,
			[]any{int64(0)},
			"_dolmen_cursor_tokens_chain_start",
		},
		{
			"reach boundary",
			`SELECT MIN(chain_origin) FROM _dolmen_cursor_tokens`,
			nil,
			"_dolmen_cursor_tokens_chain_origin",
		},
		{
			"record age prune",
			`DELETE FROM _dolmen_changes
			 WHERE at < ?
			   AND seq <= COALESCE((SELECT MIN(chain_origin) FROM _dolmen_cursor_tokens),
			                        9223372036854775807)`,
			[]any{isoChangeStamp(time.Now())},
			"_dolmen_changes_at",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := n.ro.QueryContext(ctx, `EXPLAIN QUERY PLAN `+tc.query, tc.args...)
			if err != nil {
				t.Fatalf("explain: %v", err)
			}
			defer rows.Close()
			var plan strings.Builder
			for rows.Next() {
				var id, parent, notused int
				var detail string
				if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
					t.Fatalf("scan plan row: %v", err)
				}
				plan.WriteString(detail)
				plan.WriteString("\n")
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("iterate plan: %v", err)
			}
			if !strings.Contains(plan.String(), tc.want) {
				t.Fatalf("query plan does not use %s — the replay path would scan under the write lock:\n%s", tc.want, plan.String())
			}
		})
	}
}

func TestChangesSinceEmptyPollMintsNothing(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	insertNotes(t, st, 2)
	_, head, err := st.ChangesSince(ctx, "test", "", "", [16]byte{}, nil, Incarnation{}, Page{})
	if err != nil {
		t.Fatalf("bare start: %v", err)
	}

	countTokens := func() int {
		n, err := st.ns("test")
		if err != nil {
			t.Fatalf("ns: %v", err)
		}
		var c int
		if err := n.ro.QueryRowContext(ctx, `SELECT count(*) FROM _dolmen_cursor_tokens`).Scan(&c); err != nil {
			t.Fatalf("count tokens: %v", err)
		}
		return c
	}
	before := countTokens()
	for i := 0; i < 5; i++ {
		records, next, err := st.ChangesSince(ctx, "test", "", head, [16]byte{}, nil, Incarnation{}, Page{})
		if err != nil {
			t.Fatalf("quiet poll %d: %v", i, err)
		}
		if len(records) != 0 {
			t.Fatalf("quiet poll %d delivered %d records", i, len(records))
		}
		if next != head {
			t.Fatalf("empty poll minted a new token (%q ≠ %q) — an empty page must re-present the caller's own", next, head)
		}
	}
	if after := countTokens(); after != before {
		t.Fatalf("5 quiet polls grew the token table %d → %d — an empty poll must mint nothing", before, after)
	}

	insertNotes(t, st, 1)
	records, next, err := st.ChangesSince(ctx, "test", "", head, [16]byte{}, nil, Incarnation{}, Page{})
	if err != nil {
		t.Fatalf("poll after commit: %v", err)
	}
	if len(records) != 1 || records[0].Cursor == head {
		t.Fatalf("page after commit = %+v — a real page carries a fresh per-record cursor", records)
	}
	if next == head {
		t.Fatal("non-empty page must mint a fresh next-page token")
	}
}

func TestChangesSinceRegistryWaitHonorsContext(t *testing.T) {
	st := openChangeStore(t)
	insertNotes(t, st, 1)
	st.mu.Lock()
	bounded, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := st.ChangesSince(bounded, "test", "", "", [16]byte{}, nil, Incarnation{}, Page{})
	st.mu.Unlock()
	if err == nil {
		t.Fatal("read behind a held registry lock succeeded — the context must bound the acquire")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded read error = %v, want context deadline exceeded", err)
	}
	if held := time.Since(start); held > 500*time.Millisecond {
		t.Fatalf("bounded read held %v behind the lock — the deadline must bound it", held)
	}
}

func TestNsCtxInitializationHonorsContext(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if err := st.CreateNamespace(context.Background(), "test", [16]byte{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	st2, err := Open(dir)
	if err != nil {
		t.Fatalf("open second store: %v", err)
	}
	defer st2.Close()

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st2.nsCtx(expired, "test"); !errors.Is(err, context.Canceled) {
		t.Fatalf("first open with an expired context = %v, want context.Canceled — the DDL must honor the context", err)
	}

	if _, err := st2.nsCtx(context.Background(), "test"); err != nil {
		t.Fatalf("open with a live context: %v", err)
	}
}
