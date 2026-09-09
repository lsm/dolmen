package store

import (
	"context"
	"database/sql"
	"fmt"
)

// mintChanges appends one change record per affected id to the namespace's
// durable change log (§9.3), INSIDE the caller's write transaction: the
// records and the data write they describe commit or roll back together, so
// there is no crash window in which an acknowledged write is missing from the
// log, and a rolled-back write leaves zero rows behind. The AUTOINCREMENT seq
// assignment makes each call's positions contiguous — writers serialize on the
// database's write lock, so nothing can interleave between this transaction's
// inserts — and the seq IS the per-namespace cursor (§9.3: monotonic, gap-free
// by construction).
//
// Each row carries the lifetime labels a replay needs (§3.4): the table's
// CURRENT drop generation, read inside the same transaction so a record can
// never claim a generation whose lifetime it was not minted in, and the
// namespace's creation id. owners carries each row's OWN owner label — the
// row's, never a single caller value (§9.3: the label is the row's, so a
// table-wide writer must not relabel other users' rows); nil, or a ""
// entry, mints NULL — the label until owner stamping lands (slice 9c).
//
// The returned ChangeRange identifies exactly what this call minted; the
// caller attaches it to the write result (§6.2). The records themselves are
// never materialized on the write path — a bulk write has no match-count cap,
// and a routine mutation must not construct an in-memory copy of the durable
// log it just wrote (§6.2).
func mintChanges(ctx context.Context, tx *sql.Tx, table string, kind ChangeKind, ids []int64, owners []string) (ChangeRange, error) {
	if len(ids) == 0 {
		return ChangeRange{}, nil
	}
	if owners != nil && len(owners) != len(ids) {
		return ChangeRange{}, fmt.Errorf("mint change records for %s: %d owner labels for %d ids", table, len(owners), len(ids))
	}
	gen, err := tableGen(ctx, tx, table)
	if err != nil {
		return ChangeRange{}, err
	}
	nsGen, err := readNSGen(ctx, tx)
	if err != nil {
		return ChangeRange{}, err
	}
	var first, last int64
	for i, id := range ids {
		// nil owner writes SQL NULL — the ChangeRecord convention maps the
		// empty string back to "no owner label" on read (§6.2).
		var owner any
		if owners != nil && owners[i] != "" {
			owner = owners[i]
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO _dolmen_changes(table_name, row_id, kind, owner, nsgen, drop_gen) VALUES(?,?,?,?,?,?)`,
			table, id, string(kind), owner, nsGen[:], gen)
		if err != nil {
			return ChangeRange{}, fmt.Errorf("mint change records for %s: %w", table, err)
		}
		seq, err := res.LastInsertId()
		if err != nil {
			return ChangeRange{}, err
		}
		if i == 0 {
			first = seq
		}
		last = seq
	}
	return ChangeRange{First: first, Last: last, Count: int64(len(ids))}, nil
}

// mintChangesFromTemp is mintChanges for the writes whose affected rows the
// transaction already materialized into a temp id table — _dolmen_update_ids
// for updates and upserts, _dolmen_delete_ids for deletes. The mint is a
// single INSERT…SELECT out of that table: the ids never round-trip through
// the server, so a bulk update or confirmed delete — which has no match-count
// cap — cannot consume memory proportional to the table when the request
// itself was O(1) (§6.2). Only the scalar ChangeRange crosses back, and it is
// reconstructed from the statement's row count and last-insert id without
// reading a single row back: one statement assigns the AUTOINCREMENT seqs
// contiguously in insertion order, so the two scalars pin the whole range.
// Rows are minted in id order, like every other path; an empty temp table
// mints nothing and returns the zero range.
func mintChangesFromTemp(ctx context.Context, tx *sql.Tx, table string, kind ChangeKind, temp string) (ChangeRange, error) {
	gen, err := tableGen(ctx, tx, table)
	if err != nil {
		return ChangeRange{}, err
	}
	nsGen, err := readNSGen(ctx, tx)
	if err != nil {
		return ChangeRange{}, err
	}
	res, err := tx.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO _dolmen_changes(table_name, row_id, kind, owner, nsgen, drop_gen) SELECT ?, id, ?, NULL, ?, ? FROM %s ORDER BY id`, temp),
		table, string(kind), nsGen[:], gen)
	if err != nil {
		return ChangeRange{}, fmt.Errorf("mint change records for %s: %w", table, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return ChangeRange{}, err
	}
	if n == 0 {
		return ChangeRange{}, nil
	}
	last, err := res.LastInsertId()
	if err != nil {
		return ChangeRange{}, err
	}
	return ChangeRange{First: last - n + 1, Last: last, Count: n}, nil
}
