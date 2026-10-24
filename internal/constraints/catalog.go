package constraints

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/lsm/dolmen/internal/schema"
)

type Table struct {
	Schema *schema.TableSchema
	Rows   map[int64]Row
}

type namedUnique struct {
	name string
	plan *UniquePlan
}

type tablePlan struct {
	own     bool
	unique  []namedUnique
	foreign []*ForeignPlan
}

func CheckCatalog(ctx context.Context, tables map[string]Table) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	names := slices.Sorted(maps.Keys(tables))
	plans := make(map[string]tablePlan, len(tables))
	for _, name := range names {
		sc := tables[name].Schema
		if sc == nil {
			return fmt.Errorf("constraint checking requires a schema for every table in the namespace snapshot")
		}
		if err := schema.ValidateConstraintDeclarations(ctx, sc.Fields, sc.Constraints); err != nil {
			return err
		}
		p := tablePlan{own: sc.RowAccess == schema.RowAccessOwn}
		for _, c := range sc.Constraints {
			switch c.Kind {
			case schema.ConstraintUnique:
				plan, err := CompileUnique(ctx, sc.Fields, c)
				if err != nil {
					return err
				}
				p.unique = append(p.unique, namedUnique{c.Name, plan})
			case schema.ConstraintForeignKey:
				parent, exists := tables[c.References.Table]
				if !exists || parent.Schema == nil {
					return fmt.Errorf("constraint %q: the referenced table must exist in the same namespace snapshot", c.Name)
				}
				target := ReferenceTarget{Name: c.References.Table, Own: parent.Schema.RowAccess == schema.RowAccessOwn, AcceptReferences: parent.Schema.AcceptReferences}
				plan, err := CompileForeign(ctx, sc.Fields, c, p.own, target)
				if err != nil {
					return err
				}
				p.foreign = append(p.foreign, plan)
			}
		}
		plans[name] = p
	}
	lookup := func(ctx context.Context, table string, id int64) (*string, bool, error) {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		row, found := tables[table].Rows[id]
		return row.Owner, found, nil
	}
	for _, name := range names {
		p, table := plans[name], tables[name]
		if len(p.unique) == 0 && len(p.foreign) == 0 {
			continue
		}
		ids := slices.Sorted(maps.Keys(table.Rows))
		rows := make([]Row, 0, len(ids))
		for _, id := range ids {
			rows = append(rows, table.Rows[id])
		}
		for _, c := range p.unique {
			if err := CheckUnique(ctx, c.name, c.plan, rows, p.own); err != nil {
				return err
			}
		}
		for _, c := range p.foreign {
			for _, row := range rows {
				if err := c.Check(ctx, row, lookup); err != nil {
					return err
				}
			}
		}
	}
	return ctx.Err()
}

func SelectChecks(tables map[string]Table, changed string, incoming bool) map[string]Table {
	selected := make(map[string]Table, len(tables))
	for name, t := range tables {
		if t.Schema != nil {
			sc := *t.Schema
			sc.Constraints = nil
			for _, c := range t.Schema.Constraints {
				if name == changed || incoming && c.Kind == schema.ConstraintForeignKey && c.References != nil && c.References.Table == changed {
					sc.Constraints = append(sc.Constraints, c)
				}
			}
			t.Schema = &sc
		}
		selected[name] = t
	}
	return selected
}
