package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

const LegacyIdempotencyOwner = ""

func ensureIdempotencyOwner(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(_dolmen_idempotency)`)
	if err != nil {
		return err
	}
	present := false
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "owner" {
			present = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if present {
		return nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`CREATE TABLE _dolmen_idempotency_owned(
			table_name TEXT NOT NULL,
			owner TEXT NOT NULL DEFAULT '',
			key TEXT NOT NULL,
			payload_hash TEXT NOT NULL,
			ids_json TEXT NOT NULL,
			at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			PRIMARY KEY(table_name, owner, key)
		)`,
		`INSERT INTO _dolmen_idempotency_owned(table_name, owner, key, payload_hash, ids_json, at)
			SELECT table_name, '', key, payload_hash, ids_json, at FROM _dolmen_idempotency`,
		`DROP TABLE _dolmen_idempotency`,
		`ALTER TABLE _dolmen_idempotency_owned RENAME TO _dolmen_idempotency`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("give idempotency records an owner domain: %w", err)
		}
	}
	return tx.Commit()
}

type idemDomain struct {
	owner         string
	tableWideRead bool
}

func domainFor(opts WriteOpts, scope *RowScope) idemDomain {
	return idemDomain{owner: opts.Owner, tableWideRead: opts.TableWideRead}
}

func (d idemDomain) readsLegacy() bool {
	return d.owner == LegacyIdempotencyOwner || d.tableWideRead
}

func lookupIdem(ctx context.Context, db rowQuerier, table, key, wantHash string, domain idemDomain) (ids []int64, found bool, err error) {
	ids, found, err = readIdem(ctx, db, table, domain.owner, key, wantHash)
	if err != nil || found {
		return ids, found, err
	}
	if domain.owner != LegacyIdempotencyOwner && domain.tableWideRead {
		return readIdem(ctx, db, table, LegacyIdempotencyOwner, key, wantHash)
	}
	return nil, false, nil
}

func readIdem(ctx context.Context, db rowQuerier, table, owner, key, wantHash string) (ids []int64, found bool, err error) {
	var gotHash, idsJSON string
	err = db.QueryRowContext(ctx,
		`SELECT payload_hash, ids_json FROM _dolmen_idempotency WHERE table_name = ? AND owner = ? AND key = ?`,
		table, owner, key).Scan(&gotHash, &idsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if gotHash != wantHash {
		return nil, false, conflictf("idempotency key %q was already recorded for a different insert into %s; for a retry, re-send the identical body with the same key (a client-regenerated timestamp or nonce is the classic cause; a fresh key would insert a duplicate); for a genuinely new insert, use a fresh key", key, table)
	}
	if err := json.Unmarshal([]byte(idsJSON), &ids); err != nil {
		return nil, false, fmt.Errorf("corrupt idempotency record for key %q: %w", key, err)
	}
	return ids, true, nil
}
