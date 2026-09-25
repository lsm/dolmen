package postgres

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/secret"
	"github.com/lsm/dolmen/internal/store"
)

func (s *Store) RotateSecrets(ctx context.Context, ns string, opts store.RotateOpts) (store.SecretRotation, error) {
	if err := store.RequireRotationKey(s.secrets); err != nil {
		return store.SecretRotation{}, err
	}
	var names []string
	err := s.readOnly(ctx, ns, func(tx pgx.Tx, n namespace) error {
		rows, err := tx.Query(ctx, "SELECT name FROM "+s.relation("tables")+" WHERE namespace=$1 AND active ORDER BY name", ns)
		if err != nil {
			return err
		}
		names, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		return store.SecretRotation{}, err
	}
	var out store.SecretRotation
	rotated := 0
	for _, table := range names {
		var state tableState
		var before map[string]int64
		err := s.readOnly(ctx, ns, func(tx pgx.Tx, n namespace) error {
			var err error
			if state, err = s.loadTable(ctx, tx, n, table); err != nil {
				return err
			}
			before, err = s.secretKeyCounts(ctx, tx, n, state)
			return err
		})
		if err != nil {
			return store.SecretRotation{}, err
		}
		fields := state.schema.SecretFields()
		if len(fields) == 0 {
			continue
		}
		if err := store.CheckRotationKeys(s.secrets, ns, table, before); err != nil {
			return store.SecretRotation{}, err
		}
		tr := store.SecretTableRotation{Table: table}
		for _, f := range fields {
			for !opts.Stop(rotated) {
				done, err := s.rotateBatch(ctx, ns, state, table, f.Name, opts.Batch(rotated))
				if err != nil {
					return store.SecretRotation{}, err
				}
				rotated += done
				tr.Rotated += int64(done)
				if done == 0 {
					break
				}
			}
		}
		err = s.readOnly(ctx, ns, func(tx pgx.Tx, n namespace) error {
			var err error
			tr.Keys, err = s.secretKeyCounts(ctx, tx, n, state)
			return err
		})
		if err != nil {
			return store.SecretRotation{}, err
		}
		for id, c := range tr.Keys {
			if id != s.secrets.ID() {
				tr.Remaining += c
			}
		}
		out.Tables = append(out.Tables, tr)
	}
	return out, nil
}

func (s *Store) secretKeyCounts(ctx context.Context, tx pgx.Tx, n namespace, state tableState) (map[string]int64, error) {
	keys := map[string]int64{}
	for _, f := range state.schema.SecretFields() {
		col := ident(state.columns[f.Name])
		rows, err := tx.Query(ctx, "SELECT substring("+col+" from 2 for 8), count(*) FROM "+ident(n.physical, state.physical)+" WHERE "+col+" IS NOT NULL GROUP BY 1")
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id []byte
			var c int64
			if err := rows.Scan(&id, &c); err != nil {
				rows.Close()
				return nil, err
			}
			keys[hex.EncodeToString(id)] += c
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

func (s *Store) rotateBatch(ctx context.Context, ns string, state tableState, table, field string, limit int) (int, error) {
	done := 0
	err := s.write(ctx, ns, state.incarnation.NsGen, func(tx pgx.Tx, n namespace) error {
		current, err := s.loadTable(ctx, tx, n, table)
		if err != nil {
			return err
		}
		if current.incarnation != state.incarnation || current.columns[field] != state.columns[field] {
			return nil
		}
		col := ident(state.columns[field])
		rel := ident(n.physical, state.physical)
		rows, err := tx.Query(ctx, "SELECT id, "+col+" FROM "+rel+" WHERE "+col+" IS NOT NULL AND substring("+col+" from 2 for 8) <> $1 LIMIT $2 FOR UPDATE", s.secrets.ActiveID(), limit)
		if err != nil {
			return err
		}
		type pending struct {
			id  int64
			old []byte
		}
		var batch []pending
		for rows.Next() {
			var p pending
			if err := rows.Scan(&p.id, &p.old); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, p := range batch {
			fresh, err := store.Reseal(s.secrets, field, p.old)
			if err != nil {
				if id, ok := secret.KeyID(p.old); ok {
					if cerr := store.CheckRotationKeys(s.secrets, ns, table, map[string]int64{id: 1}); cerr != nil {
						return cerr
					}
				}
				return fmt.Errorf("rotate %s.%s row %d: %w", table, field, p.id, err)
			}
			if _, err := tx.Exec(ctx, "UPDATE "+rel+" SET "+col+"=$1 WHERE id=$2", fresh, p.id); err != nil {
				return err
			}
		}
		done = len(batch)
		return nil
	})
	return done, err
}
