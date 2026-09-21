package store

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func idemStore(t *testing.T) *Store {
	t.Helper()
	st := openRowAccessStore(t)
	if _, err := st.CreateTable(context.Background(), "ns", "notes",
		[]schema.Field{{Name: "body", Type: schema.Text}}, TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	return st
}

func insertKeyed(t *testing.T, st *Store, opts WriteOpts, scope *RowScope, body string) InsertResult {
	t.Helper()
	res, err := st.Insert(context.Background(), "ns", "notes", []map[string]any{{"body": body}},
		opts, Embedder{}, scope, Incarnation{})
	if err != nil {
		t.Fatalf("insert %+v: %v", opts, err)
	}
	return res
}

func TestLegacyRecordsReplayOnlyToTableWideReaders(t *testing.T) {
	st := idemStore(t)

	legacy := insertKeyed(t, st, WriteOpts{IdempotencyKey: "k"}, nil, "written before auth")
	if legacy.Replayed {
		t.Fatal("the first insert replayed")
	}

	scoped := insertKeyed(t, st, WriteOpts{IdempotencyKey: "k", Owner: "alice"}, &RowScope{Owner: "alice"}, "alice's")
	if scoped.Replayed {
		t.Fatalf("a scoped caller replayed a legacy record, handing them ids they cannot see: %v", scoped.Ids)
	}
	if fmt.Sprint(scoped.Ids) == fmt.Sprint(legacy.Ids) {
		t.Fatalf("a scoped caller was handed the legacy record's ids: %v", scoped.Ids)
	}

	wide := insertKeyed(t, st, WriteOpts{IdempotencyKey: "k", Owner: "carol", TableWideRead: true},
		nil, "written before auth")
	if !wide.Replayed {
		t.Fatal("a table-wide reader did not replay the legacy record, though its ids are rows they can already see")
	}
	if fmt.Sprint(wide.Ids) != fmt.Sprint(legacy.Ids) {
		t.Fatalf("the table-wide replay returned %v, want the legacy ids %v", wide.Ids, legacy.Ids)
	}

	back := insertKeyed(t, st, WriteOpts{IdempotencyKey: "k"}, nil, "written before auth")
	if !back.Replayed || fmt.Sprint(back.Ids) != fmt.Sprint(legacy.Ids) {
		t.Fatalf("an auth-off retry did not replay the legacy record: replayed=%v ids=%v", back.Replayed, back.Ids)
	}
}

func TestAnAuthOffRetryNeverReplaysAnAuthOnRecord(t *testing.T) {
	st := idemStore(t)

	alice := insertKeyed(t, st, WriteOpts{IdempotencyKey: "j", Owner: "alice"}, &RowScope{Owner: "alice"}, "alice's")
	bob := insertKeyed(t, st, WriteOpts{IdempotencyKey: "j", Owner: "bob"}, &RowScope{Owner: "bob"}, "bob's")
	if bob.Replayed {
		t.Fatal("bob replayed alice's record under the same key")
	}

	off := insertKeyed(t, st, WriteOpts{IdempotencyKey: "j"}, nil, "after the switch")
	if off.Replayed {
		t.Fatalf("an auth-off retry replayed an auth-on record, which means it chose between alice's and bob's: %v", off.Ids)
	}
	for _, prior := range [][]int64{alice.Ids, bob.Ids} {
		if fmt.Sprint(off.Ids) == fmt.Sprint(prior) {
			t.Fatalf("the auth-off insert returned a principal's ids: %v", off.Ids)
		}
	}

	again := insertKeyed(t, st, WriteOpts{IdempotencyKey: "j"}, nil, "after the switch")
	if !again.Replayed || fmt.Sprint(again.Ids) != fmt.Sprint(off.Ids) {
		t.Fatalf("the auth-off record does not replay to itself: replayed=%v ids=%v", again.Replayed, again.Ids)
	}
}

func TestOwnDomainHitComparesPayloadAgainstOwnRecordOnly(t *testing.T) {
	st := idemStore(t)
	ctx := context.Background()

	insertKeyed(t, st, WriteOpts{IdempotencyKey: "p", Owner: "alice"}, &RowScope{Owner: "alice"}, "alice's")

	if _, err := st.Insert(ctx, "ns", "notes", []map[string]any{{"body": "something else"}},
		WriteOpts{IdempotencyKey: "p", Owner: "bob"}, Embedder{}, &RowScope{Owner: "bob"}, Incarnation{}); err != nil {
		t.Fatalf("a different payload under a foreign owner's key must not conflict: %v", err)
	}

	if _, err := st.Insert(ctx, "ns", "notes", []map[string]any{{"body": "something else"}},
		WriteOpts{IdempotencyKey: "p", Owner: "alice"}, Embedder{}, &RowScope{Owner: "alice"}, Incarnation{}); err == nil {
		t.Fatal("a different payload under the caller's own key was accepted")
	}
}

func TestIdempotencyRecordsGainAnOwnerColumnOnOpen(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	mustNS(t, legacy(st), "ns")
	n, err := st.ns("ns")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	ctx := context.Background()
	for _, stmt := range []string{
		`DROP TABLE _dolmen_idempotency`,
		`CREATE TABLE _dolmen_idempotency(
			table_name TEXT NOT NULL,
			key TEXT NOT NULL,
			payload_hash TEXT NOT NULL,
			ids_json TEXT NOT NULL,
			at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			PRIMARY KEY(table_name, key)
		)`,
		`INSERT INTO _dolmen_idempotency(table_name, key, payload_hash, ids_json) VALUES('notes','k','hash','[7]')`,
	} {
		if _, err := n.rw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("stage the pre-auth table: %v", err)
		}
	}
	st.Close()

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	n2, err := reopened.ns("ns")
	if err != nil {
		t.Fatalf("ns after reopen: %v", err)
	}
	var owner, idsJSON string
	if err := n2.rw.QueryRowContext(ctx,
		`SELECT owner, ids_json FROM _dolmen_idempotency WHERE table_name = 'notes' AND key = 'k'`).
		Scan(&owner, &idsJSON); err != nil {
		t.Fatalf("the pre-auth record did not survive the rebuild: %v", err)
	}
	if owner != LegacyIdempotencyOwner {
		t.Fatalf("the pre-auth record landed in domain %q, want the legacy domain", owner)
	}
	if idsJSON != "[7]" {
		t.Fatalf("the pre-auth record's ids changed: %s", idsJSON)
	}
}

func TestTheOwnerLayoutRaisesTheMinimumReader(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	mustNS(t, legacy(st), "ns")
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if minReader, ok := catalogMeta(t, dir, "ns", catalogMinReaderKey); !ok || minReader != strconv.Itoa(CatalogMinReader) {
		t.Fatalf("min reader = %q (present=%v), want %d: an older binary reads this table without the owner column and would replay one principal's ids to another",
			minReader, ok, CatalogMinReader)
	}
}

func TestAPreAuthNamespaceIsClosedToOlderBinaries(t *testing.T) {
	dir := t.TempDir()
	seedNamespace(t, dir, "legacy")
	setCatalogMeta(t, dir, "legacy", catalogFormatKey, "1")
	setCatalogMeta(t, dir, "legacy", catalogMinReaderKey, "1")

	st, err := Open(dir)
	if err != nil {
		t.Fatalf("a namespace written before the owner layout must still open: %v", err)
	}
	if _, err := st.ListTables(context.Background(), "legacy", nil); err != nil {
		t.Fatalf("list tables: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	minReader, ok := catalogMeta(t, dir, "legacy", catalogMinReaderKey)
	if !ok || minReader != strconv.Itoa(CatalogMinReader) {
		t.Fatalf("adopting a pre-auth namespace left min reader at %q (present=%v); the rebuild permits two owners under one key, which an older reader cannot see",
			minReader, ok)
	}
}

func TestASecondOpenerDoesNotRebuildTwice(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	mustNS(t, legacy(st), "ns")
	n, err := st.ns("ns")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	ctx := context.Background()
	for _, stmt := range []string{
		`DROP TABLE _dolmen_idempotency`,
		`CREATE TABLE _dolmen_idempotency(
			table_name TEXT NOT NULL,
			key TEXT NOT NULL,
			payload_hash TEXT NOT NULL,
			ids_json TEXT NOT NULL,
			at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			PRIMARY KEY(table_name, key)
		)`,
		`INSERT INTO _dolmen_idempotency(table_name, key, payload_hash, ids_json) VALUES('notes','k','hash','[1]')`,
	} {
		if _, err := n.rw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("stage the pre-auth table: %v", err)
		}
	}
	st.Close()

	first, err := Open(dir)
	if err != nil {
		t.Fatalf("first reopen: %v", err)
	}
	fn, err := first.ns("ns")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	if _, err := fn.rw.ExecContext(ctx,
		`INSERT INTO _dolmen_idempotency(table_name, owner, key, payload_hash, ids_json) VALUES('notes','alice','k','hash','[2]')`); err != nil {
		t.Fatalf("record an owner-scoped key after the rebuild: %v", err)
	}

	if err := migrateIdempotencyOwner(ctx, fn.rw); err != nil {
		t.Fatalf("a second opener that saw the old shape before another process rebuilt it failed: %v", err)
	}

	var owners int
	if err := fn.rw.QueryRowContext(ctx,
		`SELECT count(DISTINCT owner) FROM _dolmen_idempotency WHERE table_name = 'notes' AND key = 'k'`).Scan(&owners); err != nil {
		t.Fatalf("count owners: %v", err)
	}
	if owners != 2 {
		t.Fatalf("a repeated migration collapsed %d owner domains into one; the legacy record and alice's must both survive", owners)
	}
	first.Close()
}
