package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/store"
)

const namespaceRelationsSQL = "SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relkind IN ('r','m') ORDER BY c.relname"

const namespaceSizeSQL = "SELECT COALESCE(sum(pg_total_relation_size(c.oid)),0)::bigint FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relkind IN ('r','m')"

func (s *Store) Vacuum(ctx context.Context, ns string) (store.VacuumResult, error) {
	var physical string
	var relations []string
	var res store.VacuumResult
	err := s.readOnly(ctx, ns, func(tx pgx.Tx, n namespace) error {
		physical = n.physical
		if err := tx.QueryRow(ctx, namespaceSizeSQL, physical).Scan(&res.BytesBefore); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, namespaceRelationsSQL, physical)
		if err != nil {
			return err
		}
		relations, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		return store.VacuumResult{}, err
	}
	done, err := s.begin(ctx)
	if err != nil {
		return store.VacuumResult{}, err
	}
	defer done()
	for _, rel := range relations {
		if _, err := s.pool.Exec(ctx, "VACUUM "+ident(physical, rel)); err != nil {
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || ctx.Err() != nil {
				return store.VacuumResult{}, err
			}
			if pgErr.Code == "42P01" {
				continue
			}
			if pgErr.Code == "42501" {
				return store.VacuumResult{}, &derr.Error{Code: derr.Forbidden, Cause: err, Message: "vacuum of namespace " + ns + " was refused: the server's database role does not own the namespace's tables; have the operator run dolmen with the role that created them"}
			}
			return store.VacuumResult{}, &derr.Error{Code: derr.Conflict, Cause: err, Message: "vacuum of namespace " + ns + " did not finish: PostgreSQL refused it (" + pgErr.Code + "), most often because another session holds a lock on one of its tables; retry vacuum once that session finishes"}
		}
	}
	if err := s.pool.QueryRow(ctx, namespaceSizeSQL, physical).Scan(&res.BytesAfter); err != nil {
		return store.VacuumResult{}, err
	}
	return res, nil
}
