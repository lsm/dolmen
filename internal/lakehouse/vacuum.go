package lakehouse

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/apache/iceberg-go"
	"github.com/apache/iceberg-go/table"
	"github.com/lsm/dolmen/internal/store"
)

const compactionProperty = "dolmen.compaction"

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
	for ident, err := range n.catalog.ListTables(ctx, table.Identifier(strings.Split(ns, "/"))) {
		if err != nil {
			return err
		}
		state, err := loadTable(ctx, n, ns, ident[len(ident)-1])
		if err != nil {
			return err
		}
		if err := s.compact(ctx, n, state); err != nil {
			return fmt.Errorf("compact lakehouse table %s: %w", state.schema.Name, err)
		}
	}
	return nil
}

func (s *Store) compact(ctx context.Context, n *namespace, state tableState) error {
	if state.native.Metadata().CurrentSnapshot() == nil {
		return nil
	}
	tasks, err := state.native.Scan().PlanFiles(ctx)
	if err != nil {
		return err
	}
	deletes := 0
	var size int64
	for _, task := range tasks {
		deletes += len(task.DeleteFiles)
		size += task.File.FileSizeBytes()
	}
	tx := state.native.NewTransaction()
	if len(tasks) > 1 || deletes > 0 {
		if _, err := tx.RewriteDataFiles(ctx, []table.CompactionTaskGroup{{Tasks: tasks, TotalSizeBytes: size}}, table.RewriteDataFilesOptions{SnapshotProps: iceberg.Properties{compactionProperty: "1"}}); err != nil {
			return err
		}
	}
	if len(state.native.Metadata().Snapshots()) > 1 || len(tasks) > 1 || deletes > 0 {
		if err := tx.ExpireSnapshots(table.WithRetainLast(1), table.WithOlderThan(0)); err != nil {
			return err
		}
	}
	native, err := tx.Commit(ctx)
	if err != nil {
		return err
	}
	if native.MetadataLocation() != state.native.MetadataLocation() {
		return s.syncMetadata(native, n)
	}
	return nil
}
