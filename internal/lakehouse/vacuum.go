package lakehouse

import (
	"context"
	"io/fs"
	"path/filepath"

	"github.com/lsm/dolmen/internal/store"
)

func (s *Store) Vacuum(ctx context.Context, ns string) (store.VacuumResult, error) {
	var res store.VacuumResult
	err := s.withNamespace(ctx, ns, func(n *namespace) error {
		dir := filepath.Join(s.dir, namespacePath(ns))
		var err error
		if res.BytesBefore, err = treeBytes(dir); err != nil {
			return err
		}
		if err := s.maintain(ctx, n, ns); err != nil {
			return err
		}
		if _, err := n.db.ExecContext(ctx, `VACUUM`); err != nil {
			return err
		}
		if _, err := n.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			return err
		}
		res.BytesAfter, err = treeBytes(dir)
		return err
	})
	return res, err
}

func treeBytes(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func (s *Store) maintain(ctx context.Context, n *namespace, ns string) error {
	rows, err := n.db.QueryContext(ctx, `SELECT DISTINCT table_name FROM _dolmen_lakehouse_embed_stage`)
	if err != nil {
		return err
	}
	var stale []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			rows.Close()
			return err
		}
		if !s.migrateRunning(ns, table) {
			stale = append(stale, table)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, table := range stale {
		if _, err := n.db.ExecContext(ctx, `DELETE FROM _dolmen_lakehouse_embed_stage WHERE table_name = ?`, table); err != nil {
			return err
		}
	}
	return nil
}
