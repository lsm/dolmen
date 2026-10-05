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

func (s *Store) maintain(context.Context, *namespace, string) error { return nil }
