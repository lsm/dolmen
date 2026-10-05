package lakehouse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"iter"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/iceberg-go"
	"github.com/apache/iceberg-go/table"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

const embedBackfillPage = 128

type secretMove struct {
	from, to string
	drop     bool
}

type dataWork struct {
	rows        []map[string]any
	source      map[string]string
	defaults    map[string]any
	secrets     []secretMove
	embedField  string
	embedConst  string
	embedding   bool
	embedClear  bool
	provider    string
	needRewrite bool
}

func (s *Store) planData(ctx context.Context, n *namespace, state tableState, changes []schema.Change, next *schema.TableSchema, emb store.Embedder, scope *store.RowScope, plan *store.MigrationPlan) (*dataWork, error) {
	work := &dataWork{defaults: map[string]any{}}
	if state.native.Metadata().CurrentSnapshot() != nil {
		var err error
		if work.rows, err = scanRows(ctx, state, nil); err != nil {
			return nil, err
		}
	}
	visible := 0
	for _, row := range work.rows {
		if inScope(state.schema, scope, row) {
			visible++
		}
	}
	held := len(work.rows)
	source := map[string]string{}
	for _, f := range state.schema.Fields {
		source[f.Name] = f.Name
	}
	rowAccess := state.schema.RowAccess
	for _, ch := range changes {
		switch ch.Op {
		case schema.OpAddField:
			f := schema.Normalize([]schema.Field{*ch.Field})[0]
			delete(source, f.Name)
			if ch.Default != nil {
				work.defaults[f.Name] = ch.Default
				plan.BackfillRows += int64(visible)
			}
			if f.Required && ch.Default == nil && held > 0 {
				if scope != nil {
					return nil, invalidf("cannot add required field %q to a table that already holds rows (no backfill value can be supplied); add it nullable instead, or pass a default. How many rows is reported only to a caller holding read on the table", f.Name)
				}
				return nil, invalidf("cannot add required field %q to a table with %d existing rows (no backfill value can be supplied); add it nullable instead, or pass a default", f.Name, held)
			}
		case schema.OpRenameField:
			if ch.To == ch.From {
				continue
			}
			if src, ok := source[ch.From]; ok {
				source[ch.To] = src
				delete(source, ch.From)
			}
			if d, ok := work.defaults[ch.From]; ok {
				work.defaults[ch.To] = d
				delete(work.defaults, ch.From)
			}
			work.secrets = append(work.secrets, secretMove{from: ch.From, to: ch.To})
		case schema.OpDropField:
			delete(source, ch.Name)
			delete(work.defaults, ch.Name)
			work.secrets = append(work.secrets, secretMove{from: ch.Name, drop: true})
		case schema.OpSetEnum:
			src, ok := source[ch.Name]
			if !ok || len(*ch.Enum) == 0 {
				continue
			}
			counts := map[string]int64{}
			var order []string
			for _, row := range work.rows {
				v, isText := row[src].(string)
				if !isText || schema.EnumAllows(*ch.Enum, v) {
					continue
				}
				if counts[v] == 0 {
					order = append(order, v)
				}
				counts[v]++
			}
			if len(order) == 0 {
				continue
			}
			if scope != nil {
				return nil, invalidf("field %q: cannot apply this enum — rows hold values it does not allow; update those rows to a kept value first (update with set %s = ...), or keep the values in the enum. Which values, and how many rows, is reported only to a caller holding read on the table", ch.Name, ch.Name)
			}
			parts := make([]string, len(order))
			for i, v := range order {
				parts[i] = fmt.Sprintf("%q is stored by %d rows", v, counts[v])
			}
			return nil, invalidf("field %q: cannot apply this enum — %s; update those rows to a kept value first (update with set %s = ...), or keep the values in the enum", ch.Name, strings.Join(parts, ", "), ch.Name)
		case schema.OpSetShape:
			src, ok := source[ch.Name]
			if !ok || *ch.Shape == "" {
				continue
			}
			var violating int64
			var sample []int64
			for _, row := range work.rows {
				stored, isText := row[src].(string)
				if !isText || store.ShapeFits(*ch.Shape, stored) {
					continue
				}
				violating++
				if len(sample) < 5 {
					sample = append(sample, row["id"].(int64))
				}
			}
			if violating > 0 {
				return nil, store.ShapeRowsRefusal(ch.Name, *ch.Shape, violating, sample, scope != nil)
			}
		case schema.OpSetRowAccess:
			if *ch.Value && rowAccess != schema.RowAccessOwn && held > 0 {
				if scope != nil {
					return nil, invalidf("table %s already holds rows, so row_access cannot be enabled on it: no operation can write another principal's rows as that principal, so there is no honest way to assign owners to what is already there; create a new table with row_access and replay each owner's rows under their own identity, letting the server stamp them. How many rows is reported only to a caller holding read on the table", state.schema.Name)
				}
				return nil, invalidf("table %s already holds %d rows, so row_access cannot be enabled on it: no operation can write another principal's rows as that principal, so there is no honest way to assign owners to what is already there; create a new table with row_access and replay each owner's rows under their own identity, letting the server stamp them", state.schema.Name, held)
			}
			if *ch.Value {
				rowAccess = schema.RowAccessOwn
			} else {
				rowAccess = ""
			}
		}
	}
	if plan.RebuildFulltext {
		for _, row := range work.rows {
			if !inScope(state.schema, scope, row) {
				continue
			}
			for _, f := range next.FTSFields() {
				if src, ok := source[f.Name]; ok && row[src] != nil || work.defaults[f.Name] != nil {
					plan.FulltextReindexRows++
					break
				}
			}
		}
	}
	oldVec, nextVec := state.schema.VectorizeField(), next.VectorizeField()
	switch {
	case nextVec != nil && (oldVec == nil || plan.ClearsEmbeddings || source[nextVec.Name] != oldVec.Name):
		work.embedding = true
		work.embedField = nextVec.Name
		work.provider = emb.Identity
		if src, ok := source[nextVec.Name]; ok {
			work.embedField = src
			stagedRows, err := stagedVectors(ctx, n, state, emb.Identity)
			if err != nil {
				return nil, err
			}
			for _, row := range work.rows {
				text, _ := row[src].(string)
				if text == "" || !inScope(state.schema, scope, row) {
					continue
				}
				if v, ok := stagedRows[row["id"].(int64)]; ok && v.matches(text) {
					plan.StagedRows++
					continue
				}
				plan.EmbedRows++
			}
		} else {
			work.embedField = ""
			if text, ok := work.defaults[nextVec.Name].(string); ok && text != "" {
				work.embedConst = text
				plan.EmbedRows = int64(visible)
			}
		}
	case nextVec == nil && oldVec != nil:
		work.embedClear = true
	}
	work.source = source
	work.needRewrite = len(work.rows) > 0 && (len(work.defaults) > 0 || work.embedding || work.embedClear)
	return work, nil
}

type stagedVector struct {
	digest []byte
	vector []byte
}

func (v stagedVector) matches(text string) bool {
	d := sha256.Sum256([]byte(text))
	return bytes.Equal(v.digest, d[:])
}

func stagedVectors(ctx context.Context, n *namespace, state tableState, provider string) (map[int64]stagedVector, error) {
	out := map[int64]stagedVector{}
	rows, err := n.db.QueryContext(ctx, `SELECT row_id, digest, vector FROM _dolmen_lakehouse_embed_stage WHERE table_name = ? AND generation = ? AND provider = ?`, state.incarnation.Table, state.incarnation.DropGen, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var v stagedVector
		if err := rows.Scan(&id, &v.digest, &v.vector); err != nil {
			return nil, err
		}
		out[id] = v
	}
	return out, rows.Err()
}

func (s *Store) rewriteForMigration(ctx context.Context, tx *table.Transaction, state tableState, next *schema.TableSchema, work *dataWork, vectors map[int64][]byte, constant []byte, version int) error {
	staged, err := tx.StagedTable()
	if err != nil {
		return err
	}
	rows := make([]map[string]any, 0, len(work.rows))
	for _, old := range work.rows {
		row := map[string]any{"id": old["id"], "created_at": old["created_at"]}
		if v, ok := old["_embedding"]; ok {
			row["_embedding"] = v
		}
		if v, ok := old[schema.OwnerColumn]; ok && next.HasOwner {
			row[schema.OwnerColumn] = v
		}
		for _, f := range next.Fields {
			if src, ok := work.source[f.Name]; ok {
				row[f.Name] = old[src]
			}
		}
		rows = append(rows, row)
	}
	stamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	for name, raw := range work.defaults {
		f := next.Field(name)
		if f == nil {
			continue
		}
		stored, err := s.coerceField(*f, raw, stamp)
		if err != nil {
			return err
		}
		for _, row := range rows {
			row[name] = stored
		}
	}
	vecField := next.VectorizeField()
	for _, row := range rows {
		switch {
		case work.embedClear:
			delete(row, "_embedding")
		case work.embedding && constant != nil:
			row["_embedding"] = constant
		case work.embedding && vecField != nil:
			if blob, ok := vectors[row["id"].(int64)]; ok {
				row["_embedding"] = blob
			} else {
				delete(row, "_embedding")
			}
		}
		for k, v := range row {
			if v == nil {
				delete(row, k)
			}
		}
	}
	tasks, err := state.native.Scan().PlanFiles(ctx)
	if err != nil {
		return err
	}
	var oldData, oldDeletes []iceberg.DataFile
	seen := map[string]bool{}
	for _, task := range tasks {
		oldData = append(oldData, task.File)
		for _, d := range task.DeleteFiles {
			if !seen[d.FilePath()] {
				seen[d.FilePath()] = true
				oldDeletes = append(oldDeletes, d)
			}
		}
	}
	sch, err := table.SchemaToArrowSchema(staged.Schema(), nil, true, false)
	if err != nil {
		return err
	}
	var added []iceberg.DataFile
	if len(rows) > 0 {
		record, err := arrowRecord(sch, rows)
		if err != nil {
			return err
		}
		defer record.Release()
		var batches iter.Seq2[arrow.RecordBatch, error] = func(yield func(arrow.RecordBatch, error) bool) { yield(record, nil) }
		for df, err := range table.WriteRecords(ctx, staged.Table, sch, batches) {
			if err != nil {
				return err
			}
			added = append(added, df)
		}
	}
	return tx.ReplaceFiles(ctx, oldData, added, oldDeletes, iceberg.Properties{"dolmen.migration": fmt.Sprint(version)})
}

func applySecretMoves(ctx context.Context, n *namespace, state tableState, work *dataWork) error {
	tx, err := n.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	inc := state.incarnation
	for _, m := range work.secrets {
		if m.drop {
			if _, err := tx.ExecContext(ctx, `DELETE FROM _dolmen_lakehouse_secrets WHERE table_name = ? AND generation = ? AND field = ?`, inc.Table, inc.DropGen, m.from); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE _dolmen_lakehouse_secrets SET field = ? WHERE table_name = ? AND generation = ? AND field = ?`, m.to, inc.Table, inc.DropGen, m.from); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM _dolmen_lakehouse_embed_stage WHERE table_name = ? AND generation = ?`, inc.Table, inc.DropGen); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM _dolmen_lakehouse_batch_intent`); err != nil {
		return err
	}
	return tx.Commit()
}
