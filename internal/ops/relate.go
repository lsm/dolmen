package ops

import (
	"strings"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
)

func RelationRecord(sc *schema.TableSchema, source, target int64, typ string, props map[string]any) (map[string]any, error) {
	r := sc.Relation
	if r == nil {
		return nil, derr.New(derr.InvalidRequest, "table %s is not a relation table; declare relation {source, target, type} when creating it, or write ordinary rows with insert", sc.Name)
	}
	if typ == "" {
		return nil, derr.New(derr.InvalidRequest, "type is required: the edge's relation type, stored in field %s", r.Type)
	}
	rec := make(map[string]any, len(props)+3)
	for k, v := range props {
		if lk := strings.ToLower(k); lk == r.Source || lk == r.Target || lk == r.Type {
			return nil, derr.New(derr.InvalidRequest, "properties must not set %s, a field the relation fills; pass source, target and type instead", k)
		}
		rec[k] = v
	}
	rec[r.Source], rec[r.Target], rec[r.Type] = source, target, typ
	return rec, nil
}
