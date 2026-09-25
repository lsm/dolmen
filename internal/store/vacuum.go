package store

import (
	"context"
	"fmt"
	"os"
)

type VacuumResult struct {
	BytesBefore int64
	BytesAfter  int64
}

func (s *Store) Vacuum(ctx context.Context, nsName string) (VacuumResult, error) {
	n, err := s.nsCtx(ctx, nsName)
	if err != nil {
		return VacuumResult{}, err
	}
	defer n.unpin()
	path := s.nsPath(nsName)
	conn, err := n.rw.Conn(ctx)
	if err != nil {
		return VacuumResult{}, err
	}
	defer conn.Close()
	before, err := namespaceBytes(path)
	if err != nil {
		return VacuumResult{}, err
	}
	if _, err := conn.ExecContext(ctx, `VACUUM`); err != nil {
		return VacuumResult{}, fmt.Errorf("vacuum of namespace %s failed: %w; it needs free disk space about the size of the namespace file, and any open transaction on it must finish first", nsName, err)
	}
	var busy, logPages, checkpointed int
	if err := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logPages, &checkpointed); err != nil {
		return VacuumResult{}, err
	}
	after, err := namespaceBytes(path)
	if err != nil {
		return VacuumResult{}, err
	}
	return VacuumResult{BytesBefore: before, BytesAfter: after}, nil
}

func namespaceBytes(path string) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	total := fi.Size()
	wal, err := os.Stat(path + "-wal")
	if err == nil {
		total += wal.Size()
	} else if !os.IsNotExist(err) {
		return 0, err
	}
	return total, nil
}
