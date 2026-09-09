package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

// Slice 4b: insert transactions mint change records into _dolmen_changes,
// inside the same transaction as the rows (§9.3). The fixtures read the
// namespace db directly: after a commit the rows are there, contiguous, and
// labeled; a rolled-back mint leaves zero rows and no cursor gap; an
// idempotency replay mints nothing.

// changeRow is one _dolmen_changes row as read back through direct SQL.
type changeRow struct {
	seq     int64
	table   string
	rowID   int64
	kind    string
	owner   sql.NullString
	nsGen   []byte
	dropGen int64
	at      string
}

func readChanges(t *testing.T, n *nsDB) []changeRow {
	t.Helper()
	rows, err := n.ro.QueryContext(context.Background(),
		`SELECT seq, table_name, row_id, kind, owner, nsgen, drop_gen, at FROM _dolmen_changes ORDER BY seq`)
	if err != nil {
		t.Fatalf("read _dolmen_changes: %v", err)
	}
	defer rows.Close()
	var out []changeRow
	for rows.Next() {
		var r changeRow
		if err := rows.Scan(&r.seq, &r.table, &r.rowID, &r.kind, &r.owner, &r.nsGen, &r.dropGen, &r.at); err != nil {
			t.Fatalf("scan change row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate change rows: %v", err)
	}
	return out
}

// openChangeStore opens a concrete *Store with namespace test and the notes
// table — a *Store, not the legacyStore wrapper, because these tests call the
// Engine-shaped Insert/DropTable whose old signatures legacyStore overrides.
func openChangeStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	mustNS(t, legacy(st), "test")
	if _, err := st.CreateTable(context.Background(), "test", "notes", noteFields(), TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	return st
}

// TestInsertMintsContiguousChangeRecords: a committed insert leaves exactly
// one record per inserted id, with contiguous seqs (the per-namespace cursor),
// kind insert, owner NULL (no stamping until slice 9c), the namespace's
// creation id, and the table's current drop generation. A second insert
// continues the same sequence — the cursor is namespace state, not per-table
// or per-transaction.
func TestInsertMintsContiguousChangeRecords(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	nsGen, err := st.NamespaceState(ctx, "test", nil)
	if err != nil {
		t.Fatalf("namespace state: %v", err)
	}

	res, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"title": "a", "score": 1},
		{"title": "b", "score": 2},
		{"title": "c", "score": 3},
	}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if want := (ChangeRange{First: 1, Last: 3, Count: 3}); res.Changes != want {
		t.Fatalf("first insert Changes = %+v, want %+v", res.Changes, want)
	}

	res2, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"title": "d", "score": 4},
		{"title": "e", "score": 5},
	}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if want := (ChangeRange{First: 4, Last: 5, Count: 2}); res2.Changes != want {
		t.Fatalf("second insert Changes = %+v, want %+v", res2.Changes, want)
	}

	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	rows := readChanges(t, n)
	if len(rows) != 5 {
		t.Fatalf("got %d change rows after two inserts, want 5", len(rows))
	}
	allIDs := append(append([]int64{}, res.Ids...), res2.Ids...)
	for i, r := range rows {
		if r.seq != int64(i+1) {
			t.Fatalf("row %d: seq = %d, want %d — a transaction's records must be contiguous and continue the namespace cursor", i, r.seq, i+1)
		}
		if r.table != "notes" {
			t.Fatalf("row %d: table_name = %q, want notes", i, r.table)
		}
		if r.rowID != allIDs[i] {
			t.Fatalf("row %d: row_id = %d, want %d (one record per inserted id, in order)", i, r.rowID, allIDs[i])
		}
		if r.kind != string(ChangeInsert) {
			t.Fatalf("row %d: kind = %q, want %q", i, r.kind, ChangeInsert)
		}
		if r.owner.Valid {
			t.Fatalf("row %d: owner = %q, want NULL until owner stamping lands (slice 9c)", i, r.owner.String)
		}
		if string(r.nsGen) != string(nsGen[:]) {
			t.Fatalf("row %d: nsgen = %x, want the namespace creation id %x", i, r.nsGen, nsGen)
		}
		if r.dropGen != 0 {
			t.Fatalf("row %d: drop_gen = %d, want 0 for the table's first lifetime", i, r.dropGen)
		}
		if r.at == "" {
			t.Fatalf("row %d: at must be stamped by the column default", i)
		}
	}
}

// TestChangeCursorSpansTables: the seq is the PER-NAMESPACE cursor — records
// minted for a second table continue the same sequence, never a per-table
// counter.
func TestChangeCursorSpansTables(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"title": "a", "score": 1},
		{"title": "b", "score": 2},
	}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("insert notes: %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "tasks", []schema.Field{{Name: "title", Type: schema.String}}, TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("create tasks: %v", err)
	}
	res, err := st.Insert(ctx, "test", "tasks", []map[string]any{{"title": "t"}}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("insert tasks: %v", err)
	}
	if want := (ChangeRange{First: 3, Last: 3, Count: 1}); res.Changes != want {
		t.Fatalf("tasks insert Changes = %+v, want %+v — the cursor is namespace-wide, so the third record is seq 3", res.Changes, want)
	}
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	rows := readChanges(t, n)
	if len(rows) != 3 || rows[2].table != "tasks" || rows[2].seq != 3 {
		t.Fatalf("change rows = %+v, want the tasks record at seq 3 continuing the namespace sequence", rows)
	}
}

// TestInsertReplayMintsNoChangeRecords: an idempotency replay returns the
// original ids without writing — the original insert minted its records, and
// the replay must mint none (the zero ChangeRange means exactly that).
func TestInsertReplayMintsNoChangeRecords(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	recs := []map[string]any{{"title": "a", "score": 1}}
	first, err := st.Insert(ctx, "test", "notes", recs, WriteOpts{IdempotencyKey: "k1"}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if want := (ChangeRange{First: 1, Last: 1, Count: 1}); first.Changes != want {
		t.Fatalf("first insert Changes = %+v, want %+v", first.Changes, want)
	}
	again, err := st.Insert(ctx, "test", "notes", recs, WriteOpts{IdempotencyKey: "k1"}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("replayed insert: %v", err)
	}
	if !again.Replayed {
		t.Fatal("second insert with the same key must replay")
	}
	if again.Changes != (ChangeRange{}) {
		t.Fatalf("replay Changes = %+v, want the zero range — a replay mints no records", again.Changes)
	}
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	if rows := readChanges(t, n); len(rows) != 1 {
		t.Fatalf("got %d change rows after a replayed insert, want 1 — the replay must not mint", len(rows))
	}
}

// TestMintChangesRollbackLeavesNoRecords: records minted inside a transaction
// that rolls back leave zero rows AND no cursor gap — sqlite_sequence is
// transactional, so the AUTOINCREMENT counter rolls back with the rows and
// the next committed mint reuses the rolled-back positions (§9.3:
// gap-free by construction).
func TestMintChangesRollbackLeavesNoRecords(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	if _, err := mintChanges(ctx, tx, "notes", ChangeInsert, []int64{101, 102, 103}, nil); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rows := readChanges(t, n); len(rows) != 0 {
		t.Fatalf("got %d change rows after rollback, want 0 — the mint must die with the transaction", len(rows))
	}
	// The counter rolled back too: the next committed insert mints from seq 1,
	// not 4 — a rolled-back write may not leave a gap in the cursor.
	res, err := st.Insert(ctx, "test", "notes", []map[string]any{{"title": "a", "score": 1}}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("insert after rollback: %v", err)
	}
	if want := (ChangeRange{First: 1, Last: 1, Count: 1}); res.Changes != want {
		t.Fatalf("insert after rollback Changes = %+v, want %+v — the rolled-back mint must not consume cursor positions", res.Changes, want)
	}
}

// TestChangeRecordsCarryDropGeneration: each record labels the drop
// generation of the lifetime it was minted in, so a drop-and-recreated
// successor's feed is distinguishable from its predecessor's (§3.4) — and
// DropTable does not purge the log (lifetime labels, not deletion).
func TestChangeRecordsCarryDropGeneration(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{{"title": "old", "score": 1}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("first-lifetime insert: %v", err)
	}
	if err := st.DropTable(ctx, "test", "notes", Incarnation{}); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "notes", noteFields(), TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("recreate table: %v", err)
	}
	res, err := st.Insert(ctx, "test", "notes", []map[string]any{{"title": "new", "score": 2}}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("second-lifetime insert: %v", err)
	}
	if want := (ChangeRange{First: 2, Last: 2, Count: 1}); res.Changes != want {
		t.Fatalf("second-lifetime Changes = %+v, want %+v — dropping a table must not restart the namespace cursor", res.Changes, want)
	}
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	rows := readChanges(t, n)
	if len(rows) != 2 {
		t.Fatalf("got %d change rows, want 2 — DropTable must not purge change records", len(rows))
	}
	if rows[0].dropGen != 0 || rows[1].dropGen != 1 {
		t.Fatalf("drop_gen labels = [%d, %d], want [0, 1] — each record carries its own lifetime's generation", rows[0].dropGen, rows[1].dropGen)
	}
}
