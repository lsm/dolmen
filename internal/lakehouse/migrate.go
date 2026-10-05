package lakehouse

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/apache/iceberg-go"
	"github.com/apache/iceberg-go/table"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
	"github.com/lsm/dolmen/internal/value"
)

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", store.ErrInvalid, fmt.Sprintf(format, args...))
}

func (s *Store) planSchema(ctx context.Context, state tableState, changes []schema.Change, emb store.Embedder, expected store.Incarnation) (*store.MigrationPlan, *table.Transaction, error) {
	if len(changes) == 0 {
		return nil, nil, invalidf("no changes given")
	}
	if expected.Version < 0 {
		return nil, nil, invalidf("expected_version must be positive")
	}
	if err := checkExpected(state, expected, true); err != nil {
		return nil, nil, err
	}
	if state.native.Metadata().CurrentSnapshot() != nil {
		return nil, nil, fmt.Errorf("lakehouse migrations of populated tables land with mutation support")
	}
	raw, err := json.Marshal(state.schema)
	if err != nil {
		return nil, nil, err
	}
	var next *schema.TableSchema
	if err := decodeProperty(string(raw), &next); err != nil {
		return nil, nil, err
	}
	plan := &store.MigrationPlan{FromVersion: next.Version, ToVersion: next.Version + 1, Table: next, Expected: state.incarnation, Operations: []string{}}
	tx := state.native.NewTransaction()
	vectorizeChanged := false
	find := func(name string) (int, error) {
		for i, f := range next.Fields {
			if f.Name == name {
				return i, nil
			}
		}
		return -1, invalidf("field %q not found", name)
	}
	for i, ch := range changes {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if ch.Op != schema.OpAddField && ch.Default != nil {
			return nil, nil, invalidf("changes[%d]: default is only allowed on add_field", i)
		}
		if schema.TakesValue(ch.Op) && ch.Value == nil {
			return nil, nil, invalidf("changes[%d]: %s requires value", i, ch.Op)
		}
		if !schema.TakesValue(ch.Op) && ch.Value != nil {
			return nil, nil, invalidf("changes[%d]: value is only allowed on set_fulltext/set_vectorize/set_row_access", i)
		}
		if ch.Op == schema.OpSetEnum && ch.Enum == nil || ch.Op != schema.OpSetEnum && ch.Enum != nil {
			return nil, nil, invalidf("changes[%d]: enum is required only on set_enum", i)
		}
		if err := store.ShapeChangeArgs(i, ch); err != nil {
			return nil, nil, err
		}
		if next.HasOwner {
			names := []string{ch.Name, ch.To}
			if ch.Field != nil {
				names = append(names, ch.Field.Name)
			}
			for _, name := range names {
				if strings.ToLower(strings.TrimSpace(name)) == schema.OwnerColumn {
					return nil, nil, invalidf("owner is an implicit column and stays reserved even when row_access is disabled")
				}
			}
		}
		switch ch.Op {
		case schema.OpAddField:
			if ch.Field == nil {
				return nil, nil, invalidf("add_field needs a field object")
			}
			if ch.Field.Default != nil {
				return nil, nil, invalidf("add_field takes default on the change, not inside field")
			}
			field := schema.Normalize([]schema.Field{*ch.Field})[0]
			if _, err := find(field.Name); err == nil {
				return nil, nil, invalidf("field %q already exists", field.Name)
			}
			if len(next.Fields) >= store.MaxFieldsPerTable {
				return nil, nil, invalidf("migration exceeds the field limit")
			}
			if _, err := store.ValidateTableDefinition(next.Name, []schema.Field{field}); err != nil {
				return nil, nil, err
			}
			if err := store.RequireSecretKey(s.secrets, []schema.Field{field}); err != nil {
				return nil, nil, err
			}
			if ch.Default != nil {
				if field.Type == schema.Secret {
					return nil, nil, invalidf("%s", schema.SecretRefusal(field.Name, "default"))
				}
				if _, err := value.Coerce(field, ch.Default); err != nil {
					return nil, nil, invalidf("field %s default: %v", field.Name, err)
				}
			}
			update := tx.UpdateSchema(true, true).AddColumn([]string{field.Name}, physicalType(field), "", field.Required, nil)
			if err := update.Commit(); err != nil {
				return nil, nil, err
			}
			next.Fields = append(next.Fields, field)
			if field.Fulltext {
				plan.RebuildFulltext = true
			}
			if field.Vectorize {
				vectorizeChanged = true
			}
			plan.Operations = append(plan.Operations, "add_field "+field.Name)
		case schema.OpRenameField:
			index, err := find(ch.From)
			if err != nil {
				return nil, nil, err
			}
			if err := schema.ValidateIdent(ch.To, "field name"); err != nil {
				return nil, nil, invalidf("%v", err)
			}
			if _, err := find(ch.To); err == nil {
				return nil, nil, invalidf("field %q already exists", ch.To)
			}
			if err := tx.UpdateSchema(true, false).RenameColumn([]string{ch.From}, ch.To).Commit(); err != nil {
				return nil, nil, err
			}
			if next.Fields[index].Fulltext {
				plan.RebuildFulltext = true
			}
			next.Fields[index].Name = ch.To
			plan.Destructive = append(plan.Destructive, "rename_field "+ch.From+" to "+ch.To)
			plan.Operations = append(plan.Operations, "rename_field "+ch.From+" to "+ch.To)
		case schema.OpDropField:
			index, err := find(ch.Name)
			if err != nil {
				return nil, nil, err
			}
			if err := tx.UpdateSchema(true, false).DeleteColumn([]string{ch.Name}).Commit(); err != nil {
				return nil, nil, err
			}
			if next.Fields[index].Fulltext {
				plan.RebuildFulltext = true
			}
			if next.Fields[index].Vectorize {
				vectorizeChanged = true
			}
			next.Fields = slices.Delete(next.Fields, index, index+1)
			plan.Destructive = append(plan.Destructive, "drop_field "+ch.Name)
			plan.Operations = append(plan.Operations, "drop_field "+ch.Name)
		case schema.OpSetFulltext, schema.OpSetVectorize:
			index, err := find(ch.Name)
			if err != nil {
				return nil, nil, err
			}
			if ch.Op == schema.OpSetFulltext {
				if next.Fields[index].Fulltext != *ch.Value || *ch.Value {
					plan.RebuildFulltext = true
				}
				next.Fields[index].Fulltext = *ch.Value
			} else {
				if next.Fields[index].Vectorize != *ch.Value {
					vectorizeChanged = true
				}
				next.Fields[index].Vectorize = *ch.Value
			}
			plan.Operations = append(plan.Operations, fmt.Sprintf("%s %s = %t", ch.Op, ch.Name, *ch.Value))
		case schema.OpSetEnum:
			index, err := find(ch.Name)
			if err != nil {
				return nil, nil, err
			}
			if next.Fields[index].Type != schema.String {
				return nil, nil, invalidf("enum is only allowed on string fields")
			}
			next.Fields[index].Enum = nil
			if len(*ch.Enum) > 0 {
				next.Fields[index].Enum = slices.Clone(*ch.Enum)
			}
			plan.Operations = append(plan.Operations, "set_enum "+ch.Name)
		case schema.OpSetShape:
			index, err := find(ch.Name)
			if err != nil {
				return nil, nil, err
			}
			if err := store.ShapeTarget(&next.Fields[index], *ch.Shape); err != nil {
				return nil, nil, err
			}
			if err := store.ShapeDefaults(&next.Fields[index], *ch.Shape, nil); err != nil {
				return nil, nil, err
			}
			next.Fields[index].Shape = *ch.Shape
			plan.Operations = append(plan.Operations, store.ShapeOperation(ch.Name, *ch.Shape))
		case schema.OpSetRowAccess:
			if *ch.Value {
				if err := store.ValidateOwnerCollision(next.Fields); err != nil {
					return nil, nil, err
				}
				if !next.HasOwner {
					if err := tx.UpdateSchema(true, true).AddColumn([]string{schema.OwnerColumn}, iceberg.PrimitiveTypes.String, "", true, nil).Commit(); err != nil {
						return nil, nil, err
					}
				}
				next.HasOwner = true
				next.RowAccess = schema.RowAccessOwn
			} else {
				next.RowAccess = ""
			}
			plan.Operations = append(plan.Operations, fmt.Sprintf("set_row_access %t", *ch.Value))
		default:
			return nil, nil, invalidf("unknown migration op %q", ch.Op)
		}
	}
	if err := schema.ValidateForMigration(next.Fields, state.schema.Fields); err != nil {
		return nil, nil, invalidf("%v", err)
	}
	if len(plan.Destructive) > 0 && expected.Version == 0 {
		return nil, nil, invalidf("destructive changes require expected_version")
	}
	oldVector, nextVector := state.schema.VectorizeField(), next.VectorizeField()
	if vectorizeChanged && oldVector != nil {
		plan.ClearsEmbeddings = true
	}
	if vectorizeChanged && nextVector != nil {
		if emb.Embed == nil || emb.Identity == "" {
			return nil, nil, invalidf("vectorize requires an embedding provider with a reported identity")
		}
		if _, exists := state.native.Schema().FindFieldByName("_embedding"); !exists {
			if err := tx.UpdateSchema(true, false).AddColumn([]string{"_embedding"}, iceberg.PrimitiveTypes.Binary, "", false, nil).Commit(); err != nil {
				return nil, nil, err
			}
		}
		next.EmbedSpace = emb.Identity
		next.EmbedDim = 0
	}
	next.Version = plan.ToVersion
	return plan, tx, nil
}

func (s *Store) PlanMigration(ctx context.Context, ns, name string, changes []schema.Change, emb store.Embedder, expected store.Incarnation, scope *store.RowScope, scopeExpected store.Incarnation) (*store.MigrationPlan, error) {
	var plan *store.MigrationPlan
	err := s.withNamespace(ctx, ns, func(n *namespace) error {
		state, err := loadTable(ctx, n, ns, name)
		if err != nil {
			return err
		}
		if err := checkExpected(state, scopeExpected, true); err != nil {
			return err
		}
		if scope != nil && !scope.Empty && !state.schema.HasOwner {
			return invalidf("table carries no owner column")
		}
		plan, _, err = s.planSchema(ctx, state, changes, emb, expected)
		if err == nil {
			plan.DryRun = true
		}
		return err
	})
	return plan, err
}

func (s *Store) Migrate(ctx context.Context, ns, name string, changes []schema.Change, emb store.Embedder, expected store.Incarnation) (*schema.TableSchema, error) {
	var result *schema.TableSchema
	err := s.withNamespace(ctx, ns, func(n *namespace) error {
		state, err := loadTable(ctx, n, ns, name)
		if err != nil {
			return err
		}
		plan, tx, err := s.planSchema(ctx, state, changes, emb, expected)
		if err != nil {
			return err
		}
		var history []store.Migration
		if err := decodeProperty(state.native.Properties()[migrationsProperty], &history); err != nil {
			return err
		}
		record := store.Migration{ID: int64(plan.FromVersion), FromVersion: plan.FromVersion, ToVersion: plan.ToVersion, Changes: changes, At: time.Now().UTC().Format("2006-01-02T15:04:05.000Z")}
		history = append([]store.Migration{record}, history...)
		raw, err := json.Marshal(plan.Table)
		if err != nil {
			return err
		}
		logged, err := json.Marshal(history)
		if err != nil {
			return err
		}
		if err := tx.SetProperties(iceberg.Properties{schemaProperty: string(raw), migrationsProperty: string(logged)}); err != nil {
			return err
		}
		native, err := tx.Commit(ctx)
		if err != nil {
			return err
		}
		if err := s.syncMetadata(native, n); err != nil {
			return err
		}
		result = plan.Table
		return nil
	})
	return result, err
}
