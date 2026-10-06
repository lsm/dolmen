package lakehouse

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/lsm/dolmen/internal/derr"
	"os"
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
		return nil, nil, invalidf("expected_version must be a positive schema version, got %d", expected.Version)
	}
	if (expected.Table != "" || expected.NsGen != [16]byte{}) && (expected.NsGen != [16]byte{} && expected.NsGen != state.incarnation.NsGen || expected.Table != "" && expected.Table != state.incarnation.Table || expected.DropGen != state.incarnation.DropGen) {
		return nil, nil, fmt.Errorf("%w: table %s.%s was replaced; describe the current table", store.ErrNotFound, state.schema.Namespace, state.incarnation.Table)
	}
	if err := checkExpected(state, expected, true); err != nil {
		return nil, nil, err
	}
	raw, err := json.Marshal(state.schema)
	if err != nil {
		return nil, nil, err
	}
	var next *schema.TableSchema
	if err := decodeProperty(string(raw), &next); err != nil {
		return nil, nil, err
	}
	plan := &store.MigrationPlan{FromVersion: next.Version, ToVersion: next.Version + 1, Table: next, Expected: state.incarnation, ExpectedIncarnation: store.EncodeIncarnation(state.incarnation), Operations: []string{}}
	tx := state.native.NewTransaction()
	vectorizeChanged := false
	defaults := map[string]any{}
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
			var backfill any
			if ch.Default != nil {
				if field.Type == schema.Secret {
					return nil, nil, invalidf("%s", schema.SecretRefusal(field.Name, "default"))
				}
				var err error
				backfill, err = value.Coerce(field, ch.Default)
				if err != nil {
					return nil, nil, invalidf("field %s default: %v", field.Name, err)
				}
			}
			update := tx.UpdateSchema(true, true).AddColumn([]string{field.Name}, physicalType(field), "", field.Required, nil)
			if err := update.Commit(); err != nil {
				return nil, nil, err
			}
			next.Fields = append(next.Fields, field)
			defaults[field.Name] = backfill
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
			if ch.To != ch.From {
				if _, err := find(ch.To); err == nil {
					return nil, nil, invalidf("field %q already exists", ch.To)
				}
				if err := tx.UpdateSchema(true, false).RenameColumn([]string{ch.From}, ch.To).Commit(); err != nil {
					return nil, nil, err
				}
				if backfill, ok := defaults[ch.From]; ok {
					defaults[ch.To] = backfill
					delete(defaults, ch.From)
				}
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
			delete(defaults, ch.Name)
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
			if def, ok := value.StoredString(next.Fields[index].Default); ok && !schema.EnumAllows(*ch.Enum, def) {
				return nil, nil, invalidf("field %q: the declared default %q is not in the new enum (%s); keep the value, or pick a default among the allowed values", ch.Name, def, strings.Join(*ch.Enum, ", "))
			}
			if def, ok := defaults[ch.Name].(string); ok && !schema.EnumAllows(*ch.Enum, def) {
				return nil, nil, invalidf("field %q: the add_field backfill default %q is not in the new enum (%s); keep the value, or pick a backfill among the allowed values", ch.Name, def, strings.Join(*ch.Enum, ", "))
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
			if err := store.ShapeDefaults(&next.Fields[index], *ch.Shape, defaults[ch.Name]); err != nil {
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
		if err := checkScopeExpected(state, scopeExpected); err != nil {
			return err
		}
		if scope != nil && !scope.Empty && !state.schema.HasOwner {
			return invalidf("table carries no owner column")
		}
		plan, _, err = s.planSchema(ctx, state, changes, emb, expected)
		if err != nil {
			return err
		}
		if _, err := s.planData(ctx, n, state, changes, plan.Table, emb, scope, plan); err != nil {
			return err
		}
		plan.DryRun = true
		return nil
	})
	return plan, err
}

func sameTable(ns string, current, first store.Incarnation) error {
	if first.Table != "" && (first.NsGen != current.NsGen || first.Table != current.Table || first.DropGen != current.DropGen) {
		return fmt.Errorf("%w: table %s.%s was replaced; describe the current table", store.ErrNotFound, ns, current.Table)
	}
	return nil
}

type pendingEmbed struct {
	planned  *schema.TableSchema
	gen      int64
	provider string
	constant string
	ids      []int64
	texts    []string
}

func (s *Store) Migrate(ctx context.Context, ns, name string, changes []schema.Change, emb store.Embedder, expected store.Incarnation) (*schema.TableSchema, error) {
	defer s.beginMigrate(ns, name)()
	var first store.Incarnation
	for attempt := 0; attempt <= 3; attempt++ {
		var pending pendingEmbed
		err := s.withNamespaceExclusive(ctx, ns, func(n *namespace) error {
			state, err := loadTable(ctx, n, ns, name)
			if err != nil {
				return err
			}
			if err := sameTable(ns, state.incarnation, first); err != nil {
				return err
			}
			plan, _, err := s.planSchema(ctx, state, changes, emb, expected)
			if err != nil {
				return err
			}
			work, err := s.planData(ctx, n, state, changes, plan.Table, emb, nil, plan)
			if err != nil {
				return err
			}
			first = state.incarnation
			if !work.needRewrite || !work.embedding {
				return nil
			}
			pending = pendingEmbed{planned: plan.Table, gen: state.incarnation.DropGen, provider: work.provider, constant: work.embedConst}
			if work.embedField == "" {
				return nil
			}
			held, err := stagedVectors(ctx, n, state, work.provider)
			if err != nil {
				return err
			}
			for _, row := range work.rows {
				text, _ := row[work.embedField].(string)
				id := row["id"].(int64)
				if text == "" {
					continue
				}
				if v, ok := held[id]; ok && v.matches(text) {
					continue
				}
				pending.ids = append(pending.ids, id)
				pending.texts = append(pending.texts, text)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		var constant []byte
		if pending.constant != "" {
			vecs, err := store.EmbedTexts(ctx, pending.planned, name, []string{pending.constant}, emb)
			if err != nil {
				return nil, err
			}
			constant = schema.EncodeVector(vecs[0])
		}
		for start := 0; start < len(pending.ids); start += embedBackfillPage {
			end := min(start+embedBackfillPage, len(pending.ids))
			vecs, err := store.EmbedTexts(ctx, pending.planned, name, pending.texts[start:end], emb)
			if err != nil {
				return nil, err
			}
			if err := s.withNamespace(ctx, ns, func(n *namespace) error {
				return stageVectors(ctx, n, name, pending.gen, pending.provider, pending.ids[start:end], pending.texts[start:end], vecs)
			}); err != nil {
				return nil, err
			}
		}
		var result *schema.TableSchema
		retry := false
		err = s.withNamespaceExclusive(ctx, ns, func(n *namespace) error {
			state, err := loadTable(ctx, n, ns, name)
			if err != nil {
				return err
			}
			if err := sameTable(ns, state.incarnation, first); err != nil {
				return err
			}
			plan, tx, err := s.planSchema(ctx, state, changes, emb, expected)
			if err != nil {
				return err
			}
			work, err := s.planData(ctx, n, state, changes, plan.Table, emb, nil, plan)
			if err != nil {
				return err
			}
			var vectors map[int64][]byte
			if work.needRewrite && work.embedding && work.embedField != "" {
				held, err := stagedVectors(ctx, n, state, work.provider)
				if err != nil {
					return err
				}
				vectors = map[int64][]byte{}
				for _, row := range work.rows {
					text, _ := row[work.embedField].(string)
					if text == "" {
						continue
					}
					v, ok := held[row["id"].(int64)]
					if !ok || !v.matches(text) {
						retry = true
						return nil
					}
					vectors[row["id"].(int64)] = v.vector
				}
			}
			if work.needRewrite && work.embedding && work.embedConst != "" && constant == nil {
				retry = true
				return nil
			}
			result, err = s.applyMigration(ctx, n, ns, state, changes, plan, tx, work, vectors, constant)
			return err
		})
		if err != nil {
			return nil, err
		}
		if !retry {
			return result, nil
		}
	}
	return nil, derr.New(derr.Conflict, "migration of %s.%s kept losing a race with concurrent writes while backfilling embeddings; re-issue the same migrate to continue from the rows already embedded", ns, name)
}

func stageVectors(ctx context.Context, n *namespace, table string, gen int64, provider string, ids []int64, texts []string, vecs [][]float32) error {
	tx, err := n.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, id := range ids {
		d := sha256.Sum256([]byte(texts[i]))
		if _, err := tx.ExecContext(ctx, `INSERT INTO _dolmen_lakehouse_embed_stage(table_name, generation, provider, row_id, digest, vector) VALUES(?,?,?,?,?,?) ON CONFLICT(table_name, generation, provider, row_id) DO UPDATE SET digest = excluded.digest, vector = excluded.vector`, table, gen, provider, id, d[:], schema.EncodeVector(vecs[i])); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) applyMigration(ctx context.Context, n *namespace, ns string, state tableState, changes []schema.Change, plan *store.MigrationPlan, tx *table.Transaction, work *dataWork, vectors map[int64][]byte, constant []byte) (*schema.TableSchema, error) {
	var history []store.Migration
	if err := decodeProperty(state.native.Properties()[migrationsProperty], &history); err != nil {
		return nil, err
	}
	record := store.Migration{ID: int64(plan.FromVersion), FromVersion: plan.FromVersion, ToVersion: plan.ToVersion, Changes: changes, At: time.Now().UTC().Format("2006-01-02T15:04:05.000Z")}
	history = append([]store.Migration{record}, history...)
	raw, err := json.Marshal(plan.Table)
	if err != nil {
		return nil, err
	}
	logged, err := json.Marshal(history)
	if err != nil {
		return nil, err
	}
	if err := tx.SetProperties(iceberg.Properties{schemaProperty: string(raw), migrationsProperty: string(logged)}); err != nil {
		return nil, err
	}
	populated := state.native.Metadata().CurrentSnapshot() != nil
	if populated {
		if err := s.beginBatch(ctx, n, ns); err != nil {
			return nil, err
		}
	}
	fail := func(err error) (*schema.TableSchema, error) {
		if !populated {
			return nil, err
		}
		if rerr := s.rollbackBatch(context.WithoutCancel(ctx), ns); rerr != nil {
			return nil, errors.Join(err, rerr)
		}
		return nil, err
	}
	if work.needRewrite {
		if err := s.rewriteForMigration(ctx, tx, state, plan.Table, work, vectors, constant, plan.ToVersion); err != nil {
			return fail(err)
		}
	}
	native, err := tx.Commit(ctx)
	if err != nil {
		return fail(err)
	}
	if err := s.syncMetadata(native, n); err != nil {
		return fail(err)
	}
	if populated {
		if err := applySecretMoves(ctx, n, state, work); err != nil {
			return fail(err)
		}
		os.Remove(s.journalPath(ns))
	}
	return plan.Table, nil
}
