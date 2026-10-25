package store

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/lsm/dolmen/internal/graph"
	"github.com/lsm/dolmen/internal/schema"
)

type GraphAccess func(context.Context, *schema.TableSchema) (*RowScope, error)
type sqliteGraphIndex struct {
	store  *Store
	access GraphAccess
}
type sqliteGraphSnapshot struct {
	tx      *sql.Tx
	schemas map[string]*schema.TableSchema
	scopes  map[string]*RowScope
	release func()
}

func (s *Store) GraphIndex(access GraphAccess) graph.Index { return &sqliteGraphIndex{s, access} }

func (i *sqliteGraphIndex) Snapshot(ctx context.Context, namespace string) (_ graph.Snapshot, err error) {
	n, err := i.store.nsCtx(ctx, namespace)
	if err != nil {
		return nil, err
	}
	v := &sqliteGraphSnapshot{release: sync.OnceFunc(n.unpin)}
	defer func() {
		if err != nil {
			v.Close()
		}
	}()
	schemas, scopes, err := i.catalog(ctx, n.ro)
	if err != nil {
		return nil, err
	}
	for _, name := range slices.Sorted(maps.Keys(schemas)) {
		sc := schemas[name]
		if sc.Relation == nil || (scopes[name] != nil && scopes[name].Empty) {
			continue
		}
		for _, field := range []string{sc.Relation.Source, sc.Relation.Target} {
			index := "_dolmen_graph_" + name + "." + field
			var exists bool
			if err = n.ro.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='index' AND name=?)", index).Scan(&exists); err != nil {
				return nil, err
			}
			if !exists {
				if _, err = n.rw.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS "+q(index)+" ON "+q(name)+"("+q(field)+",id)"); err != nil {
					return nil, err
				}
			}
		}
	}
	tx, release, err := beginCallerTx(ctx, n.ro, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	v.tx = tx
	v.release = sync.OnceFunc(func() { release(); n.unpin() })
	v.schemas, v.scopes, err = i.catalog(ctx, tx)
	if err != nil {
		return nil, err
	}
	return v, nil
}

func (i *sqliteGraphIndex) catalog(ctx context.Context, db querier) (map[string]*schema.TableSchema, map[string]*RowScope, error) {
	schemas, err := sqliteSchemas(ctx, db)
	if err != nil {
		return nil, nil, err
	}
	scopes := map[string]*RowScope{}
	for _, name := range slices.Sorted(maps.Keys(schemas)) {
		sc := schemas[name]
		if i.access != nil {
			scopes[name], err = i.access(ctx, sc)
			if err != nil {
				return nil, nil, err
			}
		}
		if err := scopeUsable(scopes[name], sc); err != nil {
			return nil, nil, err
		}
	}
	return schemas, scopes, nil
}

func (v *sqliteGraphSnapshot) Close() error {
	v.release()
	return nil
}

func (v *sqliteGraphSnapshot) Visible(ctx context.Context, ref graph.Ref) (bool, error) {
	if v.schemas[ref.Table] == nil {
		return false, nil
	}
	statement := "SELECT EXISTS(SELECT 1 FROM " + q(ref.Table) + " WHERE id=?"
	args := []any{ref.ID}
	if clause, extra := scopeClause(v.scopes[ref.Table], ""); clause != "" {
		statement += " AND " + clause
		args = append(args, extra...)
	}
	var visible bool
	err := v.tx.QueryRowContext(ctx, statement+")", args...).Scan(&visible)
	return visible, err
}

func graphEndpoint(sc *schema.TableSchema, field string) (string, error) {
	table := ""
	for _, c := range sc.Constraints {
		if c.Kind != schema.ConstraintForeignKey || c.Field != field || c.References == nil {
			continue
		}
		if table != "" && table != c.References.Table {
			return "", invalidf("relation table %q field %q has ambiguous foreign keys; use one referenced table per endpoint", sc.Name, field)
		}
		table = c.References.Table
	}
	if table == "" {
		return "", invalidf("relation table %q field %q has no foreign key; declare a referenced table for each endpoint", sc.Name, field)
	}
	return table, nil
}

func (v *sqliteGraphSnapshot) Adjacent(ctx context.Context, node graph.Ref, direction string, limit int) ([]graph.Edge, bool, error) {
	if limit < 0 || limit > 1000 || (direction != "out" && direction != "in" && direction != "both") {
		return nil, false, invalidf("adjacency requires direction out, in or both and a candidate limit from 0 to 1000")
	}
	edges := []graph.Edge{}
	bytes := 0
	for _, name := range slices.Sorted(maps.Keys(v.schemas)) {
		sc := v.schemas[name]
		r := sc.Relation
		if r == nil || (v.scopes[name] != nil && v.scopes[name].Empty) {
			continue
		}
		source, err := graphEndpoint(sc, r.Source)
		if err != nil {
			return nil, false, err
		}
		target, err := graphEndpoint(sc, r.Target)
		if err != nil {
			return nil, false, err
		}
		clauses := []string{}
		args := []any{}
		if source == node.Table && direction != "in" {
			clauses = append(clauses, q(r.Source)+"=?")
			args = append(args, node.ID)
		}
		if target == node.Table && direction != "out" {
			clauses = append(clauses, q(r.Target)+"=?")
			args = append(args, node.ID)
		}
		if len(clauses) == 0 {
			continue
		}
		statement := fmt.Sprintf("SELECT id,%s,%s,%s FROM %s WHERE (%s)", q(r.Source), q(r.Target), q(r.Type), q(name), strings.Join(clauses, " OR "))
		if clause, extra := scopeClause(v.scopes[name], ""); clause != "" {
			statement += " AND " + clause
			args = append(args, extra...)
		}
		rows, err := v.tx.QueryContext(ctx, statement+" ORDER BY id LIMIT ?", append(args, limit-len(edges)+1)...)
		if err != nil {
			return nil, false, err
		}
		for rows.Next() {
			edge := graph.Edge{Ref: graph.Ref{Table: name}, Source: graph.Ref{Table: source}, Target: graph.Ref{Table: target}}
			if err := rows.Scan(&edge.Ref.ID, &edge.Source.ID, &edge.Target.ID, &edge.Type); err != nil {
				rows.Close()
				return nil, false, err
			}
			if len(edges) == limit {
				rows.Close()
				return edges, true, nil
			}
			bytes += len(edge.Type) + len(name) + len(source) + len(target) + 128
			if bytes > MaxQueryBytes {
				rows.Close()
				return nil, false, invalidf("graph candidate page exceeds the read byte budget; reduce the edge limit or edge type size")
			}
			edges = append(edges, edge)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, false, err
		}
	}
	return edges, false, nil
}
