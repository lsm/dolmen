package store

import (
	"context"
	"os"

	"github.com/lsm/dolmen/internal/derr"
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
		if ctx.Err() != nil || IsFull(err) {
			return VacuumResult{}, err
		}
		return VacuumResult{}, &derr.Error{Code: derr.Conflict, Cause: err, Message: "vacuum of namespace " + nsName + " did not run: another process (a second dolmen, a backup tool, a sqlite3 shell) held its file locked; nothing changed, so retry vacuum once that process lets go"}
	}
	var busy, logPages, checkpointed int
	if err := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logPages, &checkpointed); err != nil {
		return VacuumResult{}, err
	}
	if busy != 0 {
		return VacuumResult{}, derr.New(derr.Conflict, "vacuum of namespace %s rebuilt its file, but a reader held the write-ahead log open, so the log was not truncated and its space is not yet returned; retry vacuum once those reads finish (an open subscribe stream or long query counts)", nsName)
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
