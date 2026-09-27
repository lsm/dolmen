package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"

	"github.com/lsm/dolmen/internal/schema"
)

const embedStageTable = "_dolmen_embed_stage"

const embedStageDDL = `CREATE TABLE IF NOT EXISTS _dolmen_embed_stage(
	table_name TEXT NOT NULL,
	drop_gen INTEGER NOT NULL,
	row_id INTEGER NOT NULL,
	provider TEXT NOT NULL,
	digest BLOB NOT NULL,
	vector BLOB NOT NULL,
	PRIMARY KEY(table_name, drop_gen, row_id, provider, digest)
)`

type stagedVector struct {
	id     int64
	digest [32]byte
	vec    []float32
}

func textDigest(text string) [32]byte {
	return sha256.Sum256([]byte(text))
}

func ensureEmbedStage(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, embedStageDDL)
	return err
}

func embedStagePresent(ctx context.Context, db rowQuerier) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, embedStageTable).Scan(&n)
	return n > 0, err
}

func writeStagedVectors(ctx context.Context, tx *sql.Tx, table string, gen int64, provider string, batch []stagedVector) error {
	if len(batch) == 0 {
		return nil
	}
	if err := ensureEmbedStage(ctx, tx); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, fmt.Sprintf(
		`INSERT INTO %s(table_name, drop_gen, row_id, provider, digest, vector) VALUES(?,?,?,?,?,?)
		 ON CONFLICT(table_name, drop_gen, row_id, provider, digest) DO UPDATE SET vector = excluded.vector`, embedStageTable))
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, s := range batch {
		if _, err := stmt.ExecContext(ctx, table, gen, s.id, provider, s.digest[:], encodeStageVector(s.vec)); err != nil {
			return err
		}
	}
	return nil
}

func loadStagedVectors(ctx context.Context, db querier, table string, gen int64, provider string) (map[int64]stagedVector, error) {
	present, err := embedStagePresent(ctx, db)
	if err != nil || !present {
		return map[int64]stagedVector{}, err
	}
	rows, err := db.QueryContext(ctx, fmt.Sprintf(
		`SELECT row_id, digest, vector FROM %s WHERE table_name = ? AND drop_gen = ? AND provider = ?`, embedStageTable),
		table, gen, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]stagedVector{}
	for rows.Next() {
		var id int64
		var digest, blob []byte
		if err := rows.Scan(&id, &digest, &blob); err != nil {
			return nil, err
		}
		var s stagedVector
		s.id = id
		if len(digest) != sha256.Size {
			continue
		}
		copy(s.digest[:], digest)
		s.vec = decodeStageVector(blob)
		if len(s.vec) == 0 {
			continue
		}
		out[id] = s
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func deleteStagedVectors(ctx context.Context, tx *sql.Tx, table string) error {
	present, err := embedStagePresent(ctx, tx)
	if err != nil || !present {
		return err
	}
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE table_name = ?`, embedStageTable), table)
	return err
}

func deleteAllStagedVectors(ctx context.Context, tx *sql.Tx) error {
	present, err := embedStagePresent(ctx, tx)
	if err != nil || !present {
		return err
	}
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s`, embedStageTable))
	return err
}

func encodeStageVector(v []float32) []byte {
	return schema.EncodeVector(v)
}

func decodeStageVector(b []byte) []float32 {
	v, err := schema.DecodeVector(b)
	if err != nil {
		return nil
	}
	return v
}

func (s *Store) dropAbandonedStages(ctx context.Context, n *nsDB, nsName string) error {
	present, err := embedStagePresent(ctx, n.rw)
	if err != nil || !present {
		return err
	}
	tables, err := s.stageTables(ctx, n, nsName)
	if err != nil {
		return err
	}
	var stale []string
	for _, table := range tables {
		if s.migrateRunning(nsName, table) {
			continue
		}
		stale = append(stale, table)
	}
	if len(stale) == 0 {
		return nil
	}
	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range stale {
		if err := deleteStagedVectors(ctx, tx, table); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) stageTables(ctx context.Context, n *nsDB, nsName string) ([]string, error) {
	rows, err := n.ro.QueryContext(ctx, fmt.Sprintf(`SELECT DISTINCT table_name FROM %s`, embedStageTable))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			return nil, err
		}
		out = append(out, table)
	}
	return out, rows.Err()
}

func stagedCount(ctx context.Context, db querier, table string, gen int64, provider string) (int64, error) {
	present, err := embedStagePresent(ctx, db)
	if err != nil || !present {
		return 0, err
	}
	var n int64
	err = db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT count(*) FROM %s WHERE table_name = ? AND drop_gen = ? AND provider = ?`, embedStageTable),
		table, gen, provider).Scan(&n)
	return n, err
}
