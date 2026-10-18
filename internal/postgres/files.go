package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/lsm/dolmen/internal/store"
)

func (s *Store) sweepFiles(ctx context.Context, ns string, sw store.FileSweeper) (*store.FileSweep, error) {
	var swept store.FileSweep
	err := s.write(ctx, ns, [16]byte{}, func(tx pgx.Tx, n namespace) error {
		refs, err := s.fileReferences(ctx, tx, n)
		if err != nil {
			return err
		}
		swept, err = sw.Sweep(ctx, ns, refs)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &swept, nil
}

func (s *Store) fileReferences(ctx context.Context, tx pgx.Tx, n namespace) (map[string]bool, error) {
	rows, err := tx.Query(ctx, "SELECT name, physical, schema_json, columns_json, drop_generation FROM "+s.relation("tables")+" WHERE namespace=$1 AND active ORDER BY name", n.name)
	if err != nil {
		return nil, err
	}
	var states []tableState
	for rows.Next() {
		var name, physical, raw, columns string
		var generation int64
		if err := rows.Scan(&name, &physical, &raw, &columns, &generation); err != nil {
			rows.Close()
			return nil, err
		}
		st := tableState{physical: physical}
		if err := decodeTableState(&st, n, name, raw, columns, generation); err != nil {
			rows.Close()
			return nil, err
		}
		if len(st.schema.FileFields()) > 0 {
			states = append(states, st)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	refs := map[string]bool{}
	for _, st := range states {
		for _, f := range st.schema.FileFields() {
			col := ident(st.columns[f.Name])
			sums, err := tx.Query(ctx, "SELECT DISTINCT "+col+"::jsonb->>'sha256' FROM "+ident(n.physical, st.physical)+" WHERE "+col+" IS NOT NULL")
			if err != nil {
				return nil, err
			}
			for sums.Next() {
				var sum *string
				if err := sums.Scan(&sum); err != nil {
					sums.Close()
					return nil, err
				}
				if sum != nil {
					refs[*sum] = true
				}
			}
			sums.Close()
			if err := sums.Err(); err != nil {
				return nil, err
			}
		}
	}
	return refs, nil
}
