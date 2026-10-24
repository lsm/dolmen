package lakehouse

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/apache/iceberg-go/table"
	"github.com/lsm/dolmen/internal/constraints"
	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type stagedRows struct {
	deletes []int64
	rows    []map[string]any
}

func (s *Store) checkConstraints(ctx context.Context, n *namespace, state tableState, staged stagedRows, incoming bool) error {
	sc := state.schema
	if len(sc.Constraints) == 0 && !(incoming && sc.AcceptReferences) {
		return nil
	}
	states, err := s.namespaceTables(ctx, n, sc.Namespace)
	if err != nil {
		return err
	}
	states[sc.Name] = state
	tables := make(map[string]constraints.Table, len(states))
	for name, st := range states {
		tables[name] = constraints.Table{Schema: st.schema}
	}
	tables = constraints.SelectChecks(tables, sc.Name, incoming)
	needed := map[string]bool{}
	for name, t := range tables {
		if len(t.Schema.Constraints) > 0 {
			needed[name] = true
		}
		for _, c := range t.Schema.Constraints {
			if c.Kind == schema.ConstraintForeignKey && c.References != nil {
				needed[c.References.Table] = true
			}
		}
	}
	for name := range needed {
		t, ok := tables[name]
		if !ok {
			continue
		}
		raw, err := scanRows(ctx, states[name], nil)
		if err != nil {
			return err
		}
		t.Rows = make(map[int64]constraints.Row, len(raw))
		for _, row := range raw {
			r, err := constraintRow(t.Schema, row)
			if err != nil {
				return err
			}
			t.Rows[row["id"].(int64)] = r
		}
		if name == sc.Name {
			for _, id := range staged.deletes {
				delete(t.Rows, id)
			}
			for _, row := range staged.rows {
				r, err := constraintRow(t.Schema, row)
				if err != nil {
					return err
				}
				t.Rows[row["id"].(int64)] = r
			}
		}
		tables[name] = t
	}
	return catalogError(constraints.CheckCatalog(ctx, tables))
}

func catalogError(err error) error {
	if err == nil || errors.Is(err, derr.ErrConflict) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: %w", store.ErrInvalid, err)
}

func constraintRow(sc *schema.TableSchema, row map[string]any) (constraints.Row, error) {
	out := constraints.Row{Values: make(map[string]any, len(sc.Fields))}
	if owner, _ := row[schema.OwnerColumn].(string); sc.HasOwner && owner != "" {
		out.Owner = &owner
	}
	for _, f := range sc.Fields {
		v, ok := row[f.Name]
		if !ok || v == nil {
			continue
		}
		switch f.Type {
		case schema.Number:
			text, isText := v.(string)
			if !isText {
				out.Values[f.Name] = v
				continue
			}
			n, err := decodeNumber(f, text)
			if err != nil {
				return constraints.Row{}, err
			}
			out.Values[f.Name] = n
		case schema.String, schema.Text, schema.Boolean, schema.Timestamp:
			out.Values[f.Name] = v
		}
	}
	return out, nil
}

func (s *Store) namespaceTables(ctx context.Context, n *namespace, ns string) (map[string]tableState, error) {
	states := map[string]tableState{}
	for ident, err := range n.catalog.ListTables(ctx, table.Identifier(strings.Split(ns, "/"))) {
		if err != nil {
			return nil, err
		}
		name := ident[len(ident)-1]
		st, err := loadTable(ctx, n, ns, name)
		if err != nil {
			return nil, err
		}
		states[name] = st
	}
	return states, nil
}

func (s *Store) referencing(ctx context.Context, n *namespace, ns, name string) ([]string, error) {
	states, err := s.namespaceTables(ctx, n, ns)
	if err != nil {
		return nil, err
	}
	schemas := make([]*schema.TableSchema, 0, len(states))
	for _, st := range states {
		schemas = append(schemas, st.schema)
	}
	return schema.ReferencingTables(schemas, name), nil
}

func (s *Store) checkNewTable(ctx context.Context, n *namespace, sc *schema.TableSchema) error {
	if len(sc.Constraints) == 0 {
		return nil
	}
	states, err := s.namespaceTables(ctx, n, sc.Namespace)
	if err != nil {
		return err
	}
	tables := map[string]constraints.Table{sc.Name: {Schema: sc}}
	for name, st := range states {
		tables[name] = constraints.Table{Schema: st.schema}
	}
	return catalogError(constraints.CheckCatalog(ctx, constraints.SelectChecks(tables, sc.Name, false)))
}

func stampConstraintFormat(ctx context.Context, n *namespace, sc *schema.TableSchema) error {
	if len(sc.Constraints) == 0 && !sc.AcceptReferences {
		return nil
	}
	_, err := n.db.ExecContext(ctx, `UPDATE _dolmen_lakehouse_meta SET format=MAX(format, ?) WHERE singleton=1`, constraintCatalogFormat)
	return err
}

func (s *Store) checkMigratedConstraints(ctx context.Context, n *namespace, state tableState, next *schema.TableSchema) error {
	if len(next.Constraints) == 0 && !next.AcceptReferences {
		return nil
	}
	planned := state
	planned.schema = next
	return s.checkConstraints(ctx, n, planned, stagedRows{}, true)
}
