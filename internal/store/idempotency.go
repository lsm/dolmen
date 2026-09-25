package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lsm/dolmen/internal/schema"
)

const LegacyIdempotencyOwner = ""

const idempotencyTable = "_dolmen_idempotency_owned"

const legacyIdempotencyTable = "_dolmen_idempotency"

type pragmaQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func legacyIdempotencyPresent(ctx context.Context, db pragmaQuerier) (bool, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, legacyIdempotencyTable)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return false, rows.Err()
	}
	var n int
	if err := rows.Scan(&n); err != nil {
		return false, err
	}
	return n > 0, rows.Err()
}

func ensureIdempotencyOwner(ctx context.Context, db *sql.DB) error {
	present, err := legacyIdempotencyPresent(ctx, db)
	if err != nil || !present {
		return err
	}
	return migrateIdempotencyOwner(ctx, db)
}

func migrateIdempotencyOwner(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return fmt.Errorf("give idempotency records an owner domain: %w", err)
	}
	done := false
	defer func() {
		if !done {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
		}
	}()

	present, err := legacyIdempotencyPresent(ctx, conn)
	if err != nil {
		return err
	}
	if !present {
		if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
			return err
		}
		done = true
		return nil
	}

	for _, stmt := range []string{
		`INSERT INTO ` + idempotencyTable + `(table_name, owner, key, payload_hash, ids_json, at)
			SELECT table_name, '', key, payload_hash, ids_json, at FROM ` + legacyIdempotencyTable,
		`DROP TABLE ` + legacyIdempotencyTable,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("give idempotency records an owner domain: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("give idempotency records an owner domain: %w", err)
	}
	done = true
	return nil
}

type IdemDomain struct {
	Owner         string
	TableWideRead bool
}

func DomainFor(opts WriteOpts, scope *RowScope) IdemDomain {
	return IdemDomain{Owner: opts.Owner, TableWideRead: opts.TableWideRead}
}

func (d IdemDomain) FallsBackToLegacy() bool {
	return d.Owner != LegacyIdempotencyOwner && d.TableWideRead
}

func lookupIdem(ctx context.Context, db rowQuerier, table, key, wantHash string, domain IdemDomain) (ids []int64, found bool, err error) {
	ids, found, err = readIdem(ctx, db, table, domain.Owner, key, wantHash)
	if err != nil || found {
		return ids, found, err
	}
	if domain.FallsBackToLegacy() {
		return readIdem(ctx, db, table, LegacyIdempotencyOwner, key, wantHash)
	}
	return nil, false, nil
}

func readIdem(ctx context.Context, db rowQuerier, table, owner, key, wantHash string) (ids []int64, found bool, err error) {
	var gotHash, idsJSON string
	err = db.QueryRowContext(ctx,
		`SELECT payload_hash, ids_json FROM `+idempotencyTable+` WHERE table_name = ? AND owner = ? AND key = ?`,
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

const secretIdemPurgedKey = "secret_idempotency_purged"

func purgeSecretIdempotency(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var prev int
	if err := conn.QueryRowContext(ctx, `PRAGMA secure_delete`).Scan(&prev); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA secure_delete = ON`); err != nil {
		return err
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), fmt.Sprintf(`PRAGMA secure_delete = %d`, prev))
	purged, err := purgeSecretIdempotencyTx(ctx, conn)
	if err != nil || !purged {
		return err
	}
	_, _ = conn.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return nil
}

func purgeSecretIdempotencyTx(ctx context.Context, conn *sql.Conn) (bool, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var done int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM _dolmen_meta WHERE key = ?`, secretIdemPurgedKey).Scan(&done); err != nil {
		return false, err
	}
	if done > 0 {
		return false, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT name, schema_json FROM _dolmen_tables`)
	if err != nil {
		return false, err
	}
	var secretTables []string
	for rows.Next() {
		var name, raw string
		if err := rows.Scan(&name, &raw); err != nil {
			rows.Close()
			return false, err
		}
		var sc schema.TableSchema
		if err := json.Unmarshal([]byte(raw), &sc); err != nil {
			rows.Close()
			return false, fmt.Errorf("purge secret idempotency records: table %s: %w", name, err)
		}
		if len(sc.SecretFields()) > 0 {
			secretTables = append(secretTables, name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, name := range secretTables {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+idempotencyTable+` WHERE table_name = ?`, name); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO _dolmen_meta(key, value) VALUES(?, ?)`, secretIdemPurgedKey, []byte("1")); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
