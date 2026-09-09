package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

// Slice 5b: the cursor-token mapping and the retention math (§9.3), ahead of
// the ops that consume them (slice 5c). The fixtures drive the helpers
// directly with an injected clock — deadline, cap, and boundary formulas are
// pure time arithmetic and must never depend on wall-clock luck — and seed
// _dolmen_changes rows stamped at explicit times so the boundary and pruning
// math is deterministic.

// seedChanges inserts _dolmen_changes rows minted at explicit offsets from
// base (negative = before base): seqs are contiguous from 1 in slice order,
// mirroring what a run of committed writes would have minted.
func seedChanges(t *testing.T, n *nsDB, base time.Time, mintOffsets []time.Duration) {
	t.Helper()
	ctx := context.Background()
	nsGen, err := readNSGen(ctx, n.ro)
	if err != nil {
		t.Fatalf("read nsgen: %v", err)
	}
	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	for i, off := range mintOffsets {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO _dolmen_changes(table_name, row_id, kind, owner, nsgen, drop_gen, at) VALUES(?,?,?,?,?,?,?)`,
			"notes", int64(i+1), string(ChangeInsert), nil, nsGen[:], 0, isoChangeStamp(base.Add(off))); err != nil {
			t.Fatalf("seed change row %d: %v", i+1, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// changeSeqs returns the surviving change records' seqs, in order — the
// fixture-level view pruning tests assert against.
func changeSeqs(t *testing.T, n *nsDB) []int64 {
	t.Helper()
	rows := readChanges(t, n)
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.seq)
	}
	return out
}

// countTokens counts _dolmen_cursor_tokens rows.
func countTokens(t *testing.T, n *nsDB) int {
	t.Helper()
	var c int
	if err := n.ro.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM _dolmen_cursor_tokens`).Scan(&c); err != nil {
		t.Fatalf("count cursor tokens: %v", err)
	}
	return c
}

// chainOf is the inheritance mintCursorToken takes to continue a page chain:
// identity, origin, and start ride over unchanged, only the issuance is fresh.
func chainOf(row cursorRow) *cursorChain {
	return &cursorChain{ID: row.ChainID, Origin: row.ChainOrigin, Start: row.ChainStart}
}

func eqSeqs(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestChangeHead: the head position is the highest minted seq, 0 for an empty
// log — the resume position of a bare start and of head-minted tokens (§9.3:
// an omitted cursor starts at the CURRENT HEAD, no backlog).
func TestChangeHead(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	if head, err := changeHead(ctx, n.ro); err != nil || head != 0 {
		t.Fatalf("empty log head = (%d, %v), want (0, nil)", head, err)
	}
	seedChanges(t, n, time.Now(), []time.Duration{-time.Hour, -time.Minute})
	if head, err := changeHead(ctx, n.ro); err != nil || head != 2 {
		t.Fatalf("head after two mints = (%d, %v), want (2, nil)", head, err)
	}
}

// TestChangeBeginBoundary pins the "begin" boundary formula table-driven
// (§9.3): with retention R > 0 the boundary is the seq before the EARLIEST
// in-window record (MIN(seq) WHERE at >= now−R, minus 1) — everything after
// it has full page-chain headroom (M ≥ T−R). Under disordered `at` stamps
// (a clock step between commits) this formulation over-delivers interleaved
// out-of-window records rather than skipping in-window history — the
// out-of-window-MAX phrasing it replaced could skip; when nothing qualifies
// the boundary is the head (a bare start). With R = 0 the formulas must NOT
// apply — the boundary is the oldest retained record itself.
func TestChangeBeginBoundary(t *testing.T) {
	const day = 24 * time.Hour
	cases := []struct {
		name      string
		mints     []time.Duration // nil seeds an empty log
		retention time.Duration
		want      int64
	}{
		{"empty log is a bare start at head", nil, 7 * day, 0},
		{"zero retention starts at the oldest retained record", []time.Duration{-100 * day, -time.Hour}, 0, 0},
		{"everything within the window replays from the oldest", []time.Duration{-6 * day, -time.Hour, -time.Minute}, 7 * day, 0},
		{"a record exactly R old still has full headroom", []time.Duration{-7 * day, -time.Hour}, 7 * day, 0},
		{"records older than R before the call are skipped", []time.Duration{-8 * day, -6 * day, -time.Hour}, 7 * day, 1},
		{"disordered stamps never skip in-window history", []time.Duration{-time.Hour, -30 * day}, 7 * day, 0},
		{"nothing qualifies: begin is the current head", []time.Duration{-9 * day, -8 * day}, 7 * day, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := openChangeStore(t)
			n, err := st.ns("test")
			if err != nil {
				t.Fatalf("ns: %v", err)
			}
			now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
			seedChanges(t, n, now, tc.mints)
			got, err := changeBegin(context.Background(), n.ro, now, tc.retention)
			if err != nil {
				t.Fatalf("changeBegin: %v", err)
			}
			if got != tc.want {
				t.Fatalf("begin position = %d, want %d (mints %v, retention %v)", got, tc.want, tc.mints, tc.retention)
			}
		})
	}
}

// TestCursorTokenRoundTripSurvivesReopen: the mapping lives in the namespace
// db, so a token resolves identically after the store closes and reopens — a
// restart must not invalidate clients' cursors (§9.3).
func TestCursorTokenRoundTripSurvivesReopen(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	dir := st.dir
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	now := time.Now()
	tok, err := mintCursorToken(ctx, n.rw, now, 5, "notes", nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	before, err := resolveCursorToken(ctx, n.rw, now.Add(time.Second), 168*time.Hour, tok, "notes")
	if err != nil {
		t.Fatalf("resolve before reopen: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { st2.Close() })
	n2, err := st2.ns("test")
	if err != nil {
		t.Fatalf("ns after reopen: %v", err)
	}
	after, err := resolveCursorToken(ctx, n2.rw, now.Add(time.Minute), 168*time.Hour, tok, "notes")
	if err != nil {
		t.Fatalf("resolve after reopen: %v", err)
	}
	if after.Position != 5 || after.FeedTable != "notes" {
		t.Fatalf("after reopen: position/feed = %d/%q, want 5/notes", after.Position, after.FeedTable)
	}
	if after.ChainID != before.ChainID || after.ChainOrigin != before.ChainOrigin || after.ChainStart != before.ChainStart {
		t.Fatalf("chain identity changed across reopen: %+v vs %+v", after, before)
	}
}

// TestCursorTokensAreOpaqueAndFresh: a token is 16 random bytes in hex — a
// fixed 32-character string, unrelated to the position, and different on
// every issuance at the same position (fresh randomness per issuance: no
// equality to compare across polls, §9.3).
func TestCursorTokensAreOpaqueAndFresh(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	now := time.Now()
	a, err := mintCursorToken(ctx, n.rw, now, 12345, "", nil)
	if err != nil {
		t.Fatalf("mint a: %v", err)
	}
	b, err := mintCursorToken(ctx, n.rw, now, 12345, "", nil)
	if err != nil {
		t.Fatalf("mint b: %v", err)
	}
	if a == b {
		t.Fatalf("two mints at the same position yielded the same token %q — issuance must be fresh randomness", a)
	}
	for _, tok := range []Cursor{a, b} {
		if len(tok) != 2*cursorTokenBytes {
			t.Fatalf("token %q has length %d, want the fixed %d", tok, len(tok), 2*cursorTokenBytes)
		}
		if raw, err := hex.DecodeString(string(tok)); err != nil || len(raw) != cursorTokenBytes {
			t.Fatalf("token %q must be %d hex-encoded random bytes (decode: %v)", tok, cursorTokenBytes, err)
		}
	}
}

// TestResolveRejectsCrossFeedReuse: a token minted on one feed is never
// honored as a position on another — that would silently skip the target
// feed's events behind the foreign feed's cursor (§9.3).
func TestResolveRejectsCrossFeedReuse(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	now := time.Now()
	tok, err := mintCursorToken(ctx, n.rw, now, 4, "notes", nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := resolveCursorToken(ctx, n.rw, now, time.Hour, tok, ""); !errors.Is(err, ErrCursorCrossFeed) {
		t.Fatalf("resolving a table token on the namespace feed: err = %v, want ErrCursorCrossFeed", err)
	}
	if _, err := resolveCursorToken(ctx, n.rw, now, time.Hour, tok, "tasks"); !errors.Is(err, ErrCursorCrossFeed) {
		t.Fatalf("resolving a table token on another table's feed: err = %v, want ErrCursorCrossFeed", err)
	}
	if row, err := resolveCursorToken(ctx, n.rw, now, time.Hour, tok, "notes"); err != nil || row.Position != 4 {
		t.Fatalf("resolving on the minting feed = (%d, %v), want (4, nil)", row.Position, err)
	}
}

// TestCursorTokenDeadlineAndChainCap: a token resolves inside its deadline
// and dies past it; page-chain refresh carries a fresh issuance but the chain
// cap — chain_start + 2R, anchored at creation — never moves, so a late-page
// token with plenty of personal deadline left still dies at the cap (§9.3).
func TestCursorTokenDeadlineAndChainCap(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	const r = 24 * time.Hour
	t0 := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	// Own-deadline expiry needs a never-resolved token: resolving refreshes
	// the presented token's deadline, so a resolve just inside it would keep
	// it alive past it (TestResolveRefreshesPresentedToken pins that).
	inWindow, err := mintCursorToken(ctx, n.rw, t0, 3, "", nil)
	if err != nil {
		t.Fatalf("mint inWindow: %v", err)
	}
	if _, err := resolveCursorToken(ctx, n.rw, t0.Add(r-time.Minute), r, inWindow, ""); err != nil {
		t.Fatalf("resolve just inside the deadline: %v", err)
	}
	unused, err := mintCursorToken(ctx, n.rw, t0, 3, "", nil)
	if err != nil {
		t.Fatalf("mint unused: %v", err)
	}
	if _, err := resolveCursorToken(ctx, n.rw, t0.Add(r+time.Minute), r, unused, ""); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("resolve past the token's own deadline: err = %v, want ErrCursorExpired", err)
	}

	first, err := mintCursorToken(ctx, n.rw, t0, 3, "", nil)
	if err != nil {
		t.Fatalf("mint first: %v", err)
	}
	// Page 2 is issued at t0+R−5m (the client resolved `first` just inside its
	// deadline); page 3 at t0+2R−10m, still inside both next's deadline and
	// the cap. `last`'s personal deadline is t2+R — far past the cap — and the
	// cap must win over the fresh issuance.
	t1 := t0.Add(r - 5*time.Minute)
	row1, err := resolveCursorToken(ctx, n.rw, t1, r, first, "")
	if err != nil {
		t.Fatalf("resolve first at t1: %v", err)
	}
	next, err := mintCursorToken(ctx, n.rw, t1, 10, "", chainOf(row1))
	if err != nil {
		t.Fatalf("mint next: %v", err)
	}
	t2 := t0.Add(2*r - 10*time.Minute)
	row2, err := resolveCursorToken(ctx, n.rw, t2, r, next, "")
	if err != nil {
		t.Fatalf("resolve next at t2: %v", err)
	}
	last, err := mintCursorToken(ctx, n.rw, t2, 20, "", chainOf(row2))
	if err != nil {
		t.Fatalf("mint last: %v", err)
	}
	if _, err := resolveCursorToken(ctx, n.rw, t0.Add(2*r+time.Minute), r, last, ""); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("resolve past the chain cap with personal deadline to spare: err = %v, want ErrCursorExpired", err)
	}
}

// TestZeroRetentionDisablesExpiry: R = 0 disables more than pruning — no
// token deadlines, no chain cap: records never age out and a token issued
// arbitrarily long ago still resolves (§9.3).
func TestZeroRetentionDisablesExpiry(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	seedChanges(t, n, now, []time.Duration{-100 * 24 * time.Hour, -time.Hour})
	tok, err := mintCursorToken(ctx, n.rw, now.Add(-90*24*time.Hour), 1, "", nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := pruneChanges(ctx, n.rw, now, 0); err != nil {
		t.Fatalf("prune with retention 0: %v", err)
	}
	if got := changeSeqs(t, n); len(got) != 2 {
		t.Fatalf("prune with retention 0 removed records: %v, want both kept", got)
	}
	if c := countTokens(t, n); c != 1 {
		t.Fatalf("prune with retention 0 removed tokens: %d remain, want 1", c)
	}
	if row, err := resolveCursorToken(ctx, n.rw, now, 0, tok, ""); err != nil || row.Position != 1 {
		t.Fatalf("resolve a 90-day-old token with retention 0 = (%d, %v), want (1, nil)", row.Position, err)
	}
}

// TestResolveRefreshesPresentedToken: a successful resolve bumps the
// presented token's issued_at — a legitimate retry must not die because the
// first response was lost — but the chain cap stays fixed at creation, so the
// refresh extends the deadline only, never past chain_start + 2R (§9.3).
func TestResolveRefreshesPresentedToken(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	const r = time.Hour
	t0 := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	tok, err := mintCursorToken(ctx, n.rw, t0, 2, "", nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	at := t0.Add(r - 2*time.Minute)
	if _, err := resolveCursorToken(ctx, n.rw, at, r, tok, ""); err != nil {
		t.Fatalf("resolve at %v: %v", at, err)
	}
	var issued int64
	if err := n.ro.QueryRowContext(ctx,
		`SELECT issued_at FROM _dolmen_cursor_tokens WHERE token = ?`, string(tok)).Scan(&issued); err != nil {
		t.Fatalf("read issued_at: %v", err)
	}
	if issued != at.UnixMilli() {
		t.Fatalf("issued_at after resolve = %d, want the resolve time %d", issued, at.UnixMilli())
	}
	// The refreshed deadline admits a resolve past the ORIGINAL deadline...
	if _, err := resolveCursorToken(ctx, n.rw, t0.Add(r+time.Minute), r, tok, ""); err != nil {
		t.Fatalf("resolve past the original deadline after refresh: %v", err)
	}
	// ...but never past the chain's cap.
	if _, err := resolveCursorToken(ctx, n.rw, t0.Add(2*r+time.Minute), r, tok, ""); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("resolve past the chain cap: err = %v, want ErrCursorExpired", err)
	}
}

// TestResolveRefreshIsMonotonic: duplicate requests resolving the same token
// capture different nows; when an older now lands last, the stored issued_at
// must keep the newer stamp — a backward refresh would shorten the promised
// deadline and expire a boundary retry early (§9.3).
func TestResolveRefreshIsMonotonic(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	const r = time.Hour
	t0 := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	tok, err := mintCursorToken(ctx, n.rw, t0, 2, "", nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := resolveCursorToken(ctx, n.rw, t0.Add(30*time.Minute), r, tok, ""); err != nil {
		t.Fatalf("newer resolve: %v", err)
	}
	// The duplicate with the older now lands after the newer one.
	if _, err := resolveCursorToken(ctx, n.rw, t0.Add(10*time.Minute), r, tok, ""); err != nil {
		t.Fatalf("older resolve: %v", err)
	}
	var issued int64
	if err := n.ro.QueryRowContext(ctx,
		`SELECT issued_at FROM _dolmen_cursor_tokens WHERE token = ?`, string(tok)).Scan(&issued); err != nil {
		t.Fatalf("read issued_at: %v", err)
	}
	if want := t0.Add(30 * time.Minute).UnixMilli(); issued != want {
		t.Fatalf("issued_at after the older duplicate = %d, want the newer stamp %d — the refresh must never move backward", issued, want)
	}
}

// TestPruneChangesIsAgeBounded: records older than the 2R hold go, younger
// ones stay — age alone decides, never a count or byte cap (§9.3). Tokens die
// past their own deadline or their chain's cap, whichever comes first: a
// freshly-issued token on a long-dead chain is still dead.
func TestPruneChangesIsAgeBounded(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	const r = 24 * time.Hour // record hold = 2R = 48h
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	seedChanges(t, n, now, []time.Duration{
		-100 * time.Hour, -90 * time.Hour, // older than the 2R hold
		-10 * time.Hour, -9 * time.Hour, -time.Hour, // younger
	})
	// Alive: issued an hour ago. Dead: issued three retentions ago. Capped:
	// fresh issuance inheriting a chain created three retentions ago — the
	// absolute cap kills it regardless of the fresh deadline. Its origin (2)
	// must NOT protect anything once its row is gone.
	if _, err := mintCursorToken(ctx, n.rw, now.Add(-time.Hour), 5, "", nil); err != nil {
		t.Fatalf("mint alive: %v", err)
	}
	if _, err := mintCursorToken(ctx, n.rw, now.Add(-3*r), 5, "", nil); err != nil {
		t.Fatalf("mint dead: %v", err)
	}
	deadChain := &cursorChain{ID: "dead-chain", Origin: 2, Start: now.Add(-3 * r).UnixMilli()}
	if _, err := mintCursorToken(ctx, n.rw, now.Add(-time.Hour), 2, "", deadChain); err != nil {
		t.Fatalf("mint capped: %v", err)
	}
	if err := pruneChanges(ctx, n.rw, now, r); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got := changeSeqs(t, n); !eqSeqs(got, []int64{3, 4, 5}) {
		t.Fatalf("records after prune = %v, want the three age-young seqs [3 4 5] — age alone decides, never count", got)
	}
	if c := countTokens(t, n); c != 1 {
		t.Fatalf("%d tokens survive prune, want 1 (deadline and cap each kill one)", c)
	}
}

// TestPruneRetainsLiveChainRecords: a record older than the 2R hold survives
// while a live chain can still reach it (seq > chain_origin) through the
// chain's current deadline, and dies once the chain does — pure age pruning
// would break gap-free replay on a perfectly valid cursor (§9.3).
func TestPruneRetainsLiveChainRecords(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	const r = 24 * time.Hour
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	seedChanges(t, n, now, []time.Duration{-100 * time.Hour, -90 * time.Hour, -80 * time.Hour, -time.Hour})
	// A live chain resuming after seq 2: seqs 1–2 unreachable, seq 3 (80h
	// old, well past the 48h hold) retained through the chain's deadline.
	if _, err := mintCursorToken(ctx, n.rw, now.Add(-time.Hour), 2, "", nil); err != nil {
		t.Fatalf("mint live chain: %v", err)
	}
	if err := pruneChanges(ctx, n.rw, now, r); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got := changeSeqs(t, n); !eqSeqs(got, []int64{3, 4}) {
		t.Fatalf("records after prune = %v, want [3 4]: 1–2 unreachable and old, 3 old but chain-reachable, 4 young", got)
	}
	// Three retentions later the chain is long dead; everything is older than
	// the hold and nothing protects the backlog.
	if err := pruneChanges(ctx, n.rw, now.Add(3*r), r); err != nil {
		t.Fatalf("prune after the chain died: %v", err)
	}
	if got := changeSeqs(t, n); len(got) != 0 {
		t.Fatalf("records after the chain died = %v, want none", got)
	}
}

// TestSlowPageChainRidesOutPruningToTheCap is the multi-page-chain pin
// (§9.3): a chain pages slowly through a backlog that crosses the pruning
// cutoff mid-replay. Refresh and retention move together — every page
// resolves gap-free, the chain's aging backlog is retained past the 2R hold
// through the chain's deadline — until the absolute cap chain_start + 2R
// finally kills the chain and the next prune reaps the backlog with it.
func TestSlowPageChainRidesOutPruningToTheCap(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	const r = 24 * time.Hour
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	// Thirty hourly records minted now−30h..now−1h (seq s ↔ now−(31−s)h).
	mints := make([]time.Duration, 30)
	for i := range mints {
		mints[i] = -time.Duration(30-i) * time.Hour
	}
	seedChanges(t, n, now, mints)
	if head, err := changeHead(ctx, n.ro); err != nil || head != 30 {
		t.Fatalf("head = (%d, %v), want (30, nil)", head, err)
	}
	// begin at now: the oldest record with full headroom is the one minted
	// exactly R ago (seq 7), so the chain resumes after seq 6.
	begin, err := changeBegin(ctx, n.ro, now, r)
	if err != nil {
		t.Fatalf("changeBegin: %v", err)
	}
	if begin != 6 {
		t.Fatalf("begin position = %d, want 6", begin)
	}
	first, err := mintCursorToken(ctx, n.rw, now, begin, "", nil)
	if err != nil {
		t.Fatalf("mint begin token: %v", err)
	}
	if err := pruneChanges(ctx, n.rw, now, r); err != nil {
		t.Fatalf("prune at now: %v", err)
	}
	if got := changeSeqs(t, n); !eqSeqs(got, seqRange(1, 30)) {
		t.Fatalf("records after the first prune = %v, want 1..30 (nothing is past the 48h hold yet)", got)
	}

	// Page 1 delivers records 7–16; the client is slow and asks for page 2
	// only 20h later — inside `first`'s deadline.
	row1, err := resolveCursorToken(ctx, n.rw, now.Add(20*time.Hour), r, first, "")
	if err != nil {
		t.Fatalf("resolve page-1 token at now+20h: %v", err)
	}
	if row1.Position != 6 || row1.ChainOrigin != 6 || row1.ChainStart != now.UnixMilli() {
		t.Fatalf("page-1 chain = position %d origin %d start %d, want 6/6/%d", row1.Position, row1.ChainOrigin, row1.ChainStart, now.UnixMilli())
	}
	next, err := mintCursorToken(ctx, n.rw, now.Add(20*time.Hour), 16, "", chainOf(row1))
	if err != nil {
		t.Fatalf("mint page-2 token: %v", err)
	}
	if err := pruneChanges(ctx, n.rw, now.Add(20*time.Hour), r); err != nil {
		t.Fatalf("prune at now+20h: %v", err)
	}
	if got := changeSeqs(t, n); !eqSeqs(got, seqRange(3, 30)) {
		t.Fatalf("records after prune at now+20h = %v, want 3..30: seqs 1–2 past the hold, 3–6 within it", got)
	}

	// Page 2 delivers 17–26; the client is slow again — page 3 lands at
	// now+40h. The chain's records 7–22 are now past the 48h hold but the
	// chain is alive to now+48h: they must survive.
	row2, err := resolveCursorToken(ctx, n.rw, now.Add(40*time.Hour), r, next, "")
	if err != nil {
		t.Fatalf("resolve page-2 token at now+40h: %v", err)
	}
	next2, err := mintCursorToken(ctx, n.rw, now.Add(40*time.Hour), 26, "", chainOf(row2))
	if err != nil {
		t.Fatalf("mint page-3 token: %v", err)
	}
	if err := pruneChanges(ctx, n.rw, now.Add(40*time.Hour), r); err != nil {
		t.Fatalf("prune at now+40h: %v", err)
	}
	if got := changeSeqs(t, n); !eqSeqs(got, seqRange(7, 30)) {
		t.Fatalf("records after prune at now+40h = %v, want 7..30: seqs 3–6 unreachable and eligible, 7–22 past the hold but chain-retained", got)
	}

	// Page 3 drains the backlog at now+47h, just inside the cap (now+48h).
	row3, err := resolveCursorToken(ctx, n.rw, now.Add(47*time.Hour), r, next2, "")
	if err != nil {
		t.Fatalf("resolve page-3 token at now+47h: %v", err)
	}
	if row3.ChainOrigin != 6 || row3.ChainStart != now.UnixMilli() {
		t.Fatalf("chain identity drifted across pages: origin %d start %d, want 6/%d", row3.ChainOrigin, row3.ChainStart, now.UnixMilli())
	}
	last, err := mintCursorToken(ctx, n.rw, now.Add(47*time.Hour), 30, "", chainOf(row3))
	if err != nil {
		t.Fatalf("mint page-4 token: %v", err)
	}

	// Past the cap the chain dies even though `last` was issued a minute ago
	// — refresh cannot extend a chain past chain_start + 2R — and the next
	// prune reaps the chain's tokens and, with them, the backlog.
	if _, err := resolveCursorToken(ctx, n.rw, now.Add(48*time.Hour+time.Minute), r, last, ""); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("resolve past the cap: err = %v, want ErrCursorExpired", err)
	}
	if err := pruneChanges(ctx, n.rw, now.Add(49*time.Hour), r); err != nil {
		t.Fatalf("prune at now+49h: %v", err)
	}
	if got := changeSeqs(t, n); len(got) != 0 {
		t.Fatalf("records after the chain died = %v, want none", got)
	}
	if c := countTokens(t, n); c != 0 {
		t.Fatalf("%d tokens survive the final prune, want 0", c)
	}
}

// seqRange is the inclusive range lo..hi as a slice.
func seqRange(lo, hi int64) []int64 {
	out := make([]int64, 0, hi-lo+1)
	for i := lo; i <= hi; i++ {
		out = append(out, i)
	}
	return out
}

// racingPruneDB is a cursorDB double that injects the P1 interleave
// deterministically: between resolveCursorToken's SELECT and its refresh
// UPDATE, a concurrent pruneChanges deletes the token. It fires once, on the
// first UPDATE it sees, then delegates everything.
type racingPruneDB struct {
	db     cursorDB
	victim string
	fired  bool
}

func (r *racingPruneDB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return r.db.QueryRowContext(ctx, query, args...)
}

func (r *racingPruneDB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if !r.fired && strings.HasPrefix(strings.TrimSpace(query), "UPDATE") {
		r.fired = true
		if _, err := r.db.ExecContext(ctx,
			`DELETE FROM _dolmen_cursor_tokens WHERE token = ?`, r.victim); err != nil {
			return nil, err
		}
	}
	return r.db.ExecContext(ctx, query, args...)
}

// TestResolveRacingPruneReturnsExpired: a token deleted between resolve's
// SELECT and its refresh UPDATE must resolve as expired, never as a position
// — the affected-row count of the UPDATE is the existence re-check that
// makes the refresh atomic against a concurrent prune.
func TestResolveRacingPruneReturnsExpired(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	tok, err := mintCursorToken(ctx, n.rw, time.Now(), 7, "notes", nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	raced := &racingPruneDB{db: n.rw, victim: string(tok)}
	if _, err := resolveCursorToken(ctx, raced, time.Now(), time.Hour, tok, "notes"); !errors.Is(err, ErrCursorExpired) {
		t.Fatalf("resolve under a racing prune: err = %v, want ErrCursorExpired — a deleted token must not resolve", err)
	}
	if c := countTokens(t, n); c != 0 {
		t.Fatalf("fixture wiring: %d tokens remain, want the victim deleted", c)
	}
}
