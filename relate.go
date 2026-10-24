package dolmen

import (
	"context"

	"github.com/lsm/dolmen/internal/ops"
)

type Edge struct {
	Source     int64
	Target     int64
	Type       string
	Properties map[string]any
}

type RelateResult struct {
	ID       int64
	Replayed bool
	Changes  ChangeRange
}

func (s *Store) Relate(ctx context.Context, namespace, table string, edge Edge, opts InsertOptions) (RelateResult, error) {
	if err := s.begin(); err != nil {
		return RelateResult{}, err
	}
	sc, _, err := s.eng.TableState(ctx, ops.NormalizeNamespace(namespace), ops.NormalizeTable(table), nil)
	s.done()
	if err != nil {
		return RelateResult{}, facadeErr(ctx, err)
	}
	rec, err := ops.RelationRecord(sc, edge.Source, edge.Target, edge.Type, edge.Properties)
	if err != nil {
		return RelateResult{}, err
	}
	res, err := s.Insert(ctx, namespace, table, []map[string]any{rec}, opts)
	if err != nil {
		return RelateResult{}, err
	}
	return RelateResult{ID: res.Ids[0], Replayed: res.Replayed, Changes: res.Changes}, nil
}
