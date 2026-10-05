package lakehouse

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/apache/iceberg-go/table"
	"github.com/lsm/dolmen/internal/secret"
	"github.com/lsm/dolmen/internal/store"
)

func (s *Store) SecretKeyID() string { return store.ActiveSecretKeyID(s.secrets) }

func (s *Store) RotateSecrets(ctx context.Context, ns string, opts store.RotateOpts) (store.SecretRotation, error) {
	if err := store.RequireRotationKey(s.secrets); err != nil {
		return store.SecretRotation{}, err
	}
	var out store.SecretRotation
	err := s.withNamespace(ctx, ns, func(n *namespace) error {
		var states []tableState
		for ident, err := range n.catalog.ListTables(ctx, table.Identifier(strings.Split(ns, "/"))) {
			if err != nil {
				return err
			}
			state, err := loadTable(ctx, n, ns, ident[len(ident)-1])
			if err != nil {
				return err
			}
			if len(state.schema.SecretFields()) > 0 {
				states = append(states, state)
			}
		}
		slices.SortFunc(states, func(a, b tableState) int { return strings.Compare(a.schema.Name, b.schema.Name) })
		rotated := 0
		for _, state := range states {
			name := state.schema.Name
			tr := store.SecretTableRotation{Table: name}
			before, err := secretKeyCounts(ctx, n, state)
			if err != nil {
				return err
			}
			if err := store.CheckRotationKeys(s.secrets, ns, name, before); err != nil {
				return err
			}
			for _, f := range state.schema.SecretFields() {
				for !opts.Stop(rotated) {
					done, err := s.rotateBatch(ctx, n, ns, state, f.Name, opts.Batch(rotated))
					if err != nil {
						return err
					}
					rotated += done
					tr.Rotated += int64(done)
					if done == 0 {
						break
					}
				}
			}
			if tr.Keys, err = secretKeyCounts(ctx, n, state); err != nil {
				return err
			}
			for id, c := range tr.Keys {
				if id != s.secrets.ID() {
					tr.Remaining += c
				}
			}
			out.Tables = append(out.Tables, tr)
		}
		return nil
	})
	return out, err
}

func secretKeyCounts(ctx context.Context, n *namespace, state tableState) (map[string]int64, error) {
	keys := map[string]int64{}
	rows, err := n.db.QueryContext(ctx, `SELECT lower(hex(substr(value, 2, 8))), count(*) FROM _dolmen_lakehouse_secrets WHERE table_name = ? AND generation = ? GROUP BY 1`, state.incarnation.Table, state.incarnation.DropGen)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var c int64
		if err := rows.Scan(&id, &c); err != nil {
			return nil, err
		}
		keys[id] += c
	}
	return keys, rows.Err()
}

func (s *Store) rotateBatch(ctx context.Context, n *namespace, ns string, state tableState, field string, limit int) (int, error) {
	tx, err := n.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	inc := state.incarnation
	rows, err := tx.QueryContext(ctx, `SELECT row_id, value FROM _dolmen_lakehouse_secrets WHERE table_name = ? AND generation = ? AND field = ? AND substr(value, 2, 8) != ? LIMIT ?`, inc.Table, inc.DropGen, field, s.secrets.ActiveID(), limit)
	if err != nil {
		return 0, err
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
			return 0, err
		}
		batch = append(batch, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, p := range batch {
		fresh, err := store.Reseal(s.secrets, field, p.old)
		if err != nil {
			if id, ok := secret.KeyID(p.old); ok {
				if cerr := store.CheckRotationKeys(s.secrets, ns, inc.Table, map[string]int64{id: 1}); cerr != nil {
					return 0, cerr
				}
			}
			return 0, fmt.Errorf("rotate %s.%s row %d: %w", inc.Table, field, p.id, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE _dolmen_lakehouse_secrets SET value = ? WHERE table_name = ? AND generation = ? AND row_id = ? AND field = ?`, fresh, inc.Table, inc.DropGen, p.id, field); err != nil {
			return 0, err
		}
	}
	return len(batch), tx.Commit()
}
