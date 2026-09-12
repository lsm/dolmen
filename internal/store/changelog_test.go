package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

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

	res, err := st.Insert(ctx, "test", "notes", []map[string]any{{"title": "a", "score": 1}}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("insert after rollback: %v", err)
	}
	if want := (ChangeRange{First: 1, Last: 1, Count: 1}); res.Changes != want {
		t.Fatalf("insert after rollback Changes = %+v, want %+v — the rolled-back mint must not consume cursor positions", res.Changes, want)
	}
}

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

func mustChangesNS(t *testing.T, st *Store) *nsDB {
	t.Helper()
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	return n
}

func TestUpsertByKeyMintsPerBranch(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()

	first, err := st.UpsertByKey(ctx, "test", "notes", []string{"title"},
		[]map[string]any{{"title": "a", "score": 1}}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if first.Inserted != 1 || first.Updated != 0 {
		t.Fatalf("first upsert counts = inserted %d, updated %d, want 1/0", first.Inserted, first.Updated)
	}
	if want := (ChangeRange{First: 1, Last: 1, Count: 1}); first.Changes != want {
		t.Fatalf("first upsert Changes = %+v, want %+v", first.Changes, want)
	}
	idA := first.Ids[0]

	mixed, err := st.UpsertByKey(ctx, "test", "notes", []string{"title"},
		[]map[string]any{
			{"title": "a", "score": 10},
			{"title": "b", "score": 2},
		}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("mixed upsert: %v", err)
	}
	if mixed.Inserted != 1 || mixed.Updated != 1 {
		t.Fatalf("mixed upsert counts = inserted %d, updated %d, want 1/1", mixed.Inserted, mixed.Updated)
	}

	if len(mixed.Ids) != 2 || mixed.Ids[0] != idA || mixed.Ids[1] == idA {
		t.Fatalf("mixed upsert ids = %v, want [row A id %d, new id]", mixed.Ids, idA)
	}
	if want := (ChangeRange{First: 2, Last: 3, Count: 2}); mixed.Changes != want {
		t.Fatalf("mixed upsert Changes = %+v, want %+v — both branches' records in one contiguous range", mixed.Changes, want)
	}

	rows := readChanges(t, mustChangesNS(t, st))
	if len(rows) != 3 {
		t.Fatalf("got %d change rows, want 3", len(rows))
	}

	idB := mixed.Ids[1]
	want := []struct {
		kind  string
		rowID int64
	}{
		{string(ChangeInsert), idA},
		{string(ChangeInsert), idB},
		{string(ChangeUpdate), idA},
	}
	for i, w := range want {
		if rows[i].kind != w.kind || rows[i].rowID != w.rowID {
			t.Fatalf("change row %d = kind %q row_id %d, want kind %q row_id %d", i, rows[i].kind, rows[i].rowID, w.kind, w.rowID)
		}
		if rows[i].owner.Valid {
			t.Fatalf("change row %d: owner = %q, want NULL until owner stamping lands (slice 9c)", i, rows[i].owner.String)
		}
	}

	upd, err := st.UpsertByKey(ctx, "test", "notes", []string{"title"},
		[]map[string]any{{"title": "a", "score": 20}}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("pure-update upsert: %v", err)
	}
	if upd.Updated != 1 || upd.Inserted != 0 {
		t.Fatalf("pure-update counts = inserted %d, updated %d, want 0/1", upd.Inserted, upd.Updated)
	}
	if want := (ChangeRange{First: 4, Last: 4, Count: 1}); upd.Changes != want {
		t.Fatalf("pure-update Changes = %+v, want %+v", upd.Changes, want)
	}
	if rows := readChanges(t, mustChangesNS(t, st)); rows[3].kind != string(ChangeUpdate) {
		t.Fatalf("pure-update record kind = %q, want %q", rows[3].kind, ChangeUpdate)
	}
}

func TestUpsertByKeyMidBatchFailureRollsBackMint(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()

	if _, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"title": "x", "score": 7},
		{"title": "y", "score": 1},
		{"title": "y", "score": 2},
	}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	_, err := st.UpsertByKey(ctx, "test", "notes", []string{"title"},
		[]map[string]any{
			{"title": "x", "score": 99},
			{"title": "y", "score": 3},
		}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err == nil {
		t.Fatal("upsert with an ambiguous natural key must fail")
	}

	n := mustChangesNS(t, st)
	if rows := readChanges(t, n); len(rows) != 3 {
		t.Fatalf("got %d change rows after the failed batch, want 3 — the update record must die with the transaction", len(rows))
	}
	var score int64
	if err := n.ro.QueryRowContext(ctx, `SELECT score FROM notes WHERE title = 'x'`).Scan(&score); err != nil {
		t.Fatalf("read row x: %v", err)
	}
	if score != 7 {
		t.Fatalf("row x score = %d, want 7 — the first record's write must roll back with the batch", score)
	}

	res, err := st.Update(ctx, "test", "notes", "title = 'x'", nil, map[string]any{"score": 8}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("update after rollback: %v", err)
	}
	if want := (ChangeRange{First: 4, Last: 4, Count: 1}); res.Changes != want {
		t.Fatalf("update after rollback Changes = %+v, want %+v — the failed batch must not consume cursor positions", res.Changes, want)
	}
}

func TestUpdateMintsRecordsForMatchedRows(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	ins, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"title": "a", "score": 1},
		{"title": "b", "score": 2},
		{"title": "c", "score": 3},
	}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	res, err := st.Update(ctx, "test", "notes", "score >= 2", nil, map[string]any{"done": true}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if res.Updated != 2 {
		t.Fatalf("updated = %d, want 2", res.Updated)
	}
	if want := (ChangeRange{First: 4, Last: 5, Count: 2}); res.Changes != want {
		t.Fatalf("update Changes = %+v, want %+v", res.Changes, want)
	}

	bulk, err := st.Update(ctx, "test", "notes", "1=1", nil, map[string]any{"score": 9}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("bulk update: %v", err)
	}
	if bulk.Updated != 3 {
		t.Fatalf("bulk updated = %d, want 3", bulk.Updated)
	}
	if want := (ChangeRange{First: 6, Last: 8, Count: 3}); bulk.Changes != want {
		t.Fatalf("bulk update Changes = %+v, want %+v", bulk.Changes, want)
	}

	miss, err := st.Update(ctx, "test", "notes", "score = 12345", nil, map[string]any{"done": false}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("no-match update: %v", err)
	}
	if miss.Updated != 0 || miss.Changes != (ChangeRange{}) {
		t.Fatalf("no-match update = %+v, want zero count and zero range", miss)
	}

	rows := readChanges(t, mustChangesNS(t, st))
	if len(rows) != 8 {
		t.Fatalf("got %d change rows, want 8", len(rows))
	}

	for i, id := range []int64{ins.Ids[1], ins.Ids[2]} {
		r := rows[3+i]
		if r.kind != string(ChangeUpdate) || r.rowID != id {
			t.Fatalf("filtered update record %d = kind %q row_id %d, want update/%d", i, r.kind, r.rowID, id)
		}
	}

	for i, id := range ins.Ids {
		r := rows[5+i]
		if r.kind != string(ChangeUpdate) || r.rowID != id {
			t.Fatalf("bulk update record %d = kind %q row_id %d, want update/%d", i, r.kind, r.rowID, id)
		}
	}
}

func TestUpsertMintsOnBothBranches(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()

	ins, err := st.Upsert(ctx, "test", "notes", "title = 'a'", nil,
		map[string]any{"title": "a", "score": 1}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("upsert insert branch: %v", err)
	}
	if ins.Inserted != 1 || ins.Updated != 0 {
		t.Fatalf("insert branch counts = inserted %d, updated %d, want 1/0", ins.Inserted, ins.Updated)
	}
	if want := (ChangeRange{First: 1, Last: 1, Count: 1}); ins.Changes != want {
		t.Fatalf("insert branch Changes = %+v, want %+v", ins.Changes, want)
	}

	upd, err := st.Upsert(ctx, "test", "notes", "title = 'a'", nil,
		map[string]any{"score": 2}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("upsert update branch: %v", err)
	}
	if upd.Updated != 1 || upd.Inserted != 0 {
		t.Fatalf("update branch counts = inserted %d, updated %d, want 0/1", upd.Inserted, upd.Updated)
	}
	if want := (ChangeRange{First: 2, Last: 2, Count: 1}); upd.Changes != want {
		t.Fatalf("update branch Changes = %+v, want %+v", upd.Changes, want)
	}

	rows := readChanges(t, mustChangesNS(t, st))
	if len(rows) != 2 {
		t.Fatalf("got %d change rows, want 2", len(rows))
	}
	if rows[0].kind != string(ChangeInsert) || rows[0].rowID != ins.Ids[0] {
		t.Fatalf("record 0 = kind %q row_id %d, want insert/%d", rows[0].kind, rows[0].rowID, ins.Ids[0])
	}
	if rows[1].kind != string(ChangeUpdate) || rows[1].rowID != ins.Ids[0] {
		t.Fatalf("record 1 = kind %q row_id %d, want update/%d", rows[1].kind, rows[1].rowID, ins.Ids[0])
	}
}

func TestDeleteMintsDeleteRecords(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()
	ins, err := st.Insert(ctx, "test", "notes", []map[string]any{
		{"title": "a", "score": 1},
		{"title": "b", "score": 2},
		{"title": "c", "score": 3},
	}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	dry, err := st.Delete(ctx, "test", "notes", "1=1", nil, DeleteOpts{DryRun: true}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry.Matched != 3 || dry.Deleted != 0 || dry.Changes != (ChangeRange{}) {
		t.Fatalf("dry run = %+v, want matched 3, deleted 0, zero range", dry)
	}
	if rows := readChanges(t, mustChangesNS(t, st)); len(rows) != 3 {
		t.Fatalf("got %d change rows after dry run, want 3", len(rows))
	}

	if _, err := st.Delete(ctx, "test", "notes", "1=1", nil, DeleteOpts{Limit: 2}, nil, Incarnation{}); err == nil {
		t.Fatal("delete over the limit without confirm must fail")
	}
	if rows := readChanges(t, mustChangesNS(t, st)); len(rows) != 3 {
		t.Fatalf("got %d change rows after the rejected delete, want 3 — the error path must mint nothing", len(rows))
	}

	del, err := st.Delete(ctx, "test", "notes", "1=1", nil, DeleteOpts{Limit: 2, Confirm: true}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("confirmed delete: %v", err)
	}
	if del.Deleted != 3 {
		t.Fatalf("deleted = %d, want 3", del.Deleted)
	}
	if want := (ChangeRange{First: 4, Last: 6, Count: 3}); del.Changes != want {
		t.Fatalf("confirmed delete Changes = %+v, want %+v", del.Changes, want)
	}

	rows := readChanges(t, mustChangesNS(t, st))
	if len(rows) != 6 {
		t.Fatalf("got %d change rows, want 6", len(rows))
	}
	for i, id := range ins.Ids {
		r := rows[3+i]
		if r.kind != string(ChangeDelete) || r.rowID != id {
			t.Fatalf("delete record %d = kind %q row_id %d, want delete/%d", i, r.kind, r.rowID, id)
		}
		if r.owner.Valid {
			t.Fatalf("delete record %d: owner = %q, want NULL until owner stamping lands (slice 9c)", i, r.owner.String)
		}
	}

	miss, err := st.Delete(ctx, "test", "notes", "1=1", nil, DeleteOpts{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("no-match delete: %v", err)
	}
	if miss.Matched != 0 || miss.Deleted != 0 || miss.Changes != (ChangeRange{}) {
		t.Fatalf("no-match delete = %+v, want zeroes and the zero range", miss)
	}
}

func TestChangeLabelsDistinguishLifetimesAcrossPaths(t *testing.T) {
	st := openChangeStore(t)
	ctx := context.Background()

	ins, err := st.Insert(ctx, "test", "notes", []map[string]any{{"title": "a", "score": 1}}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := st.Update(ctx, "test", "notes", "id = ?", []any{ins.Ids[0]}, map[string]any{"score": 2}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := st.Delete(ctx, "test", "notes", "id = ?", []any{ins.Ids[0]}, DeleteOpts{}, nil, Incarnation{}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := st.DropTable(ctx, "test", "notes", Incarnation{}); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if _, err := st.CreateTable(ctx, "test", "notes", noteFields(), TableOpts{}, [16]byte{}); err != nil {
		t.Fatalf("recreate table: %v", err)
	}

	res, err := st.UpsertByKey(ctx, "test", "notes", []string{"title"},
		[]map[string]any{{"title": "b", "score": 1}}, WriteOpts{}, Embedder{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("upsert insert: %v", err)
	}
	if _, err := st.UpsertByKey(ctx, "test", "notes", []string{"title"},
		[]map[string]any{{"title": "b", "score": 2}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	if _, err := st.Delete(ctx, "test", "notes", "1=1", nil, DeleteOpts{}, nil, Incarnation{}); err != nil {
		t.Fatalf("delete second lifetime: %v", err)
	}
	if res.Changes.First != 4 || res.Changes.Count != 1 {
		t.Fatalf("second-lifetime upsert Changes = %+v, want First 4 Count 1 — dropping a table must not restart the namespace cursor", res.Changes)
	}

	rows := readChanges(t, mustChangesNS(t, st))
	want := []struct {
		kind    string
		dropGen int64
	}{
		{string(ChangeInsert), 0},
		{string(ChangeUpdate), 0},
		{string(ChangeDelete), 0},
		{string(ChangeInsert), 1},
		{string(ChangeUpdate), 1},
		{string(ChangeDelete), 1},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d change rows, want %d — DropTable must not purge change records", len(rows), len(want))
	}
	for i, w := range want {
		if rows[i].kind != w.kind {
			t.Fatalf("change row %d kind = %q, want %q", i, rows[i].kind, w.kind)
		}
		if rows[i].dropGen != w.dropGen {
			t.Fatalf("change row %d (kind %s) drop_gen = %d, want %d — each record carries its own lifetime's generation", i, rows[i].kind, rows[i].dropGen, w.dropGen)
		}
	}
}
