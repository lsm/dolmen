package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lsm/dolmen/internal/schema"
)

const MaxKeyFields = 8

func (s *Store) UpsertByKey(ctx context.Context, nsName, table string, keyFields []string, records []map[string]any, opts WriteOpts, emb Embedder, scope *RowScope, scopeIncarnation Incarnation) (InsertResult, error) {
	if err := s.guardIncarnation(ctx, nsName, table, scopeIncarnation); err != nil {
		return InsertResult{}, err
	}
	if len(records) == 0 {
		return InsertResult{}, invalidf("no records given")
	}
	if len(records) > MaxRecordsPerInsert {
		return InsertResult{}, invalidf("too many records: %d > %d per call", len(records), MaxRecordsPerInsert)
	}
	keyFields, err := normalizeKeyFields(keyFields)
	if err != nil {
		return InsertResult{}, err
	}
	normalized := make([]map[string]any, len(records))
	for i, rec := range records {
		nr := make(map[string]any, len(rec))
		for k, v := range rec {
			lk := strings.ToLower(k)
			if _, exists := nr[lk]; exists {
				return InsertResult{}, invalidf("record %d: fields %q and its case variant collapse to %q; use one spelling", i, k, lk)
			}
			nr[lk] = v
		}
		normalized[i] = nr
	}
	records = normalized

	n, err := s.ns(nsName)
	if err != nil {
		return InsertResult{}, err
	}
	for attempt := 0; ; attempt++ {
		if attempt >= 3 {
			return InsertResult{}, invalidf("table schema changed concurrently; retry the upsert")
		}
		ids, inserted, updated, changes, done, err := s.upsertKeyAttempt(ctx, n, nsName, table, keyFields, records, emb, opts.Owner, scope, scopeIncarnation)
		if done {
			if err != nil {
				return InsertResult{}, err
			}
			return InsertResult{Ids: ids, Inserted: int64(inserted), Updated: int64(updated), Changes: changes}, nil
		}
	}
}

func NormalizeKeyFields(fields []string) ([]string, error) { return normalizeKeyFields(fields) }

func normalizeKeyFields(keyFields []string) ([]string, error) {
	if len(keyFields) == 0 {
		return nil, invalidf("upsert needs at least one key field")
	}
	if len(keyFields) > MaxKeyFields {
		return nil, invalidf("too many key fields: %d > %d", len(keyFields), MaxKeyFields)
	}
	out := make([]string, len(keyFields))
	seen := map[string]bool{}
	for i, k := range keyFields {
		lk := strings.ToLower(strings.TrimSpace(k))

		if !schema.ValidIdentSyntax(lk) {
			return nil, invalidf("invalid key field %q: must match ^[a-z][a-z0-9_]{0,63}$", k)
		}
		if seen[lk] {
			return nil, invalidf("duplicate key field %q", lk)
		}
		seen[lk] = true
		out[i] = lk
	}
	return out, nil
}

type upsertPlan struct {
	rec     map[string]any
	cols    []string
	vals    []any
	keyVals []any
}

func matchByKey(ctx context.Context, tx *sql.Tx, table string, keyFields []string, keyDefs []*schema.Field, keyVals []any, recIdx int, scope *RowScope, sc *schema.TableSchema) (int64, string, error) {
	where := make([]string, len(keyDefs))
	for j, kd := range keyDefs {
		where[j] = fmt.Sprintf(`%s = ?`, q(kd.Name))
	}
	whereSQL := strings.Join(where, ` AND `)
	prefix, source, scopeArgs := scopedSource(table, scope)
	rows, err := tx.QueryContext(ctx,
		fmt.Sprintf(`%sSELECT id, %s FROM %s WHERE %s LIMIT 2`, prefix, changeOwnerColumn(sc), source, whereSQL),
		append(append([]any{}, scopeArgs...), keyVals...)...)
	if err != nil {
		return 0, "", NewFilterError(whereSQL, err)
	}
	var matchIDs []int64
	var owners []string
	for rows.Next() {
		var id int64
		var owner sql.NullString
		if err := rows.Scan(&id, &owner); err != nil {
			rows.Close()
			return 0, "", err
		}
		matchIDs = append(matchIDs, id)
		owners = append(owners, owner.String)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, "", err
	}
	rows.Close()
	if len(matchIDs) > 1 {
		return 0, "", conflictf("record %d: natural key (%s) matches multiple existing rows (ids %d and %d); the key is not unique in the table — delete the duplicate rows before upserting", recIdx, strings.Join(keyFields, ", "), matchIDs[0], matchIDs[1])
	}
	if len(matchIDs) == 1 {
		return matchIDs[0], owners[0], nil
	}
	return 0, "", nil
}

func (s *Store) upsertKeyAttempt(ctx context.Context, n *nsDB, nsName, table string, keyFields []string, records []map[string]any, emb Embedder, owner string, scope *RowScope, scopeIncarnation Incarnation) (ids []int64, inserted, updated int, changes ChangeRange, done bool, err error) {

	gen, err := tableGen(ctx, n.rw, table)
	if err != nil {
		return nil, 0, 0, ChangeRange{}, true, err
	}
	sc, err := loadSchema(ctx, n.rw, nsName, table)
	if err != nil {
		return nil, 0, 0, ChangeRange{}, true, err
	}
	persistMeta := sc.EmbedSpace == "" || sc.EmbedDim == 0
	origEmbedSpace := sc.EmbedSpace
	origEmbedDim := sc.EmbedDim

	keyDefs := make([]*schema.Field, len(keyFields))
	for i, name := range keyFields {
		f := sc.Field(name)
		if f == nil {
			return nil, 0, 0, ChangeRange{}, true, invalidf("key field %q is not a field of table %s (see describe_table)", name, table)
		}
		switch f.Type {
		case schema.String, schema.Text, schema.Number, schema.Boolean, schema.Timestamp:
		default:
			return nil, 0, 0, ChangeRange{}, true, invalidf("key field %q has type %s; natural keys must be string, text, number, boolean, or timestamp fields (vector and json values do not compare reliably)", name, f.Type)
		}
		keyDefs[i] = f
	}

	for _, rec := range records {
		for k := range rec {
			if sc.Field(k) == nil {
				return nil, 0, 0, ChangeRange{}, true, invalidf("unknown field %q on table %s (see describe_table)", k, table)
			}
		}
	}

	plans := make([]upsertPlan, len(records))
	for i, rec := range records {
		p := upsertPlan{rec: rec, keyVals: make([]any, len(keyDefs))}
		var cols []string
		var vals []any
		for _, f := range sc.Fields {
			v, present := rec[f.Name]
			if !present {
				continue
			}
			cv, err := coerceValue(f, v)
			if err != nil {
				return nil, 0, 0, ChangeRange{}, true, fmt.Errorf("%w: %w", ErrInvalid, err)
			}
			for j, kd := range keyDefs {
				if kd.Name == f.Name {
					p.keyVals[j] = cv
				}
			}
			cols = append(cols, q(f.Name))
			vals = append(vals, cv)
		}
		for j, kd := range keyDefs {
			if p.keyVals[j] == nil {
				return nil, 0, 0, ChangeRange{}, true, invalidf("record %d: key field %q must be present and non-null (NULL never matches an existing row, so the record would always insert)", i, kd.Name)
			}
		}
		p.cols = cols
		p.vals = vals
		plans[i] = p
	}

	embFor := map[int][]float32{}
	clearEmb := map[int]bool{}
	if vf := sc.VectorizeField(); vf != nil {
		var texts []string
		var idx []int
		for i, rec := range records {
			v, present := rec[vf.Name]
			if !present {
				continue
			}
			if t, ok := v.(string); ok && t != "" {
				texts = append(texts, t)
				idx = append(idx, i)
			} else {

				clearEmb[i] = true
			}
		}
		if len(texts) > 0 {
			vecs, err := embedTexts(ctx, sc, table, texts, emb)
			if err != nil {
				return nil, 0, 0, ChangeRange{}, true, err
			}
			for k, i := range idx {
				embFor[i] = vecs[k]
			}
		}
	}

	fts := sc.FTSFields()
	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, 0, ChangeRange{}, true, err
	}
	defer tx.Rollback()

	scTx, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return nil, 0, 0, ChangeRange{}, true, err
	}
	txGen, err := tableGen(ctx, tx, table)
	if err != nil {
		return nil, 0, 0, ChangeRange{}, true, err
	}
	if scTx.Version != sc.Version || scTx.EmbedSpace != origEmbedSpace || scTx.EmbedDim != origEmbedDim || txGen != gen {
		return nil, 0, 0, ChangeRange{}, false, nil
	}
	if err := checkScopeIncarnation(ctx, tx, nsName, table, scopeIncarnation); err != nil {
		return nil, 0, 0, ChangeRange{}, true, err
	}
	if err := scopeUsable(scope, scTx); err != nil {
		return nil, 0, 0, ChangeRange{}, true, err
	}

	ids = make([]int64, 0, len(records))
	insertIDs := make([]int64, 0, len(records))
	updateIDs := make([]int64, 0, len(records))
	updateOwners := make([]string, 0, len(records))
	for i := range plans {
		p := plans[i]
		matchID, matchOwner, err := matchByKey(ctx, tx, table, keyFields, keyDefs, p.keyVals, i, scope, sc)
		if err != nil {
			return nil, 0, 0, ChangeRange{}, true, err
		}
		if matchID == 0 {

			for _, f := range sc.Fields {
				if _, present := p.rec[f.Name]; present {
					continue
				}
				if f.Default == nil {
					continue
				}
				dv := defaultForWrite(f)
				cv, err := coerceValue(f, dv)
				if err != nil {
					return nil, 0, 0, ChangeRange{}, true, fmt.Errorf("%w: %w", ErrInvalid, err)
				}
				p.rec[f.Name] = dv
				p.cols = append(p.cols, q(f.Name))
				p.vals = append(p.vals, cv)
			}
			for _, f := range sc.Fields {
				v, present := p.rec[f.Name]
				if present && v != nil {
					continue
				}
				if f.Required {
					return nil, 0, 0, ChangeRange{}, true, invalidf("record %d: field %q is required (no existing row matched the natural key, so this record inserts)", i, f.Name)
				}
			}
			var vec []float32
			if v, ok := embFor[i]; ok {
				vec = v
			}
			id, err := execInsertWithFTS(ctx, tx, table, fts, p.rec, p.cols, p.vals, vec, stampOwner(sc, owner))
			if err != nil {
				return nil, 0, 0, ChangeRange{}, true, err
			}
			ids = append(ids, id)
			insertIDs = append(insertIDs, id)
			inserted++
			continue
		}

		for _, f := range sc.Fields {
			if !f.Required {
				continue
			}
			if v, present := p.rec[f.Name]; present && v == nil {
				return nil, 0, 0, ChangeRange{}, true, invalidf("record %d: field %q is required and cannot be set to null", i, f.Name)
			}
		}
		uCols := p.cols
		uVals := p.vals
		if vec, ok := embFor[i]; ok {
			uCols = append(append([]string(nil), p.cols...), `"_embedding"`)
			uVals = append(append([]any(nil), p.vals...), schema.EncodeVector(vec))
		} else if clearEmb[i] {
			uCols = append(append([]string(nil), p.cols...), `"_embedding"`)
			uVals = append(append([]any(nil), p.vals...), nil)
		}
		if len(uCols) > 0 {
			set := make([]string, len(uCols))
			for c, col := range uCols {
				set[c] = col + ` = ?`
			}
			uVals = append(uVals, matchID)
			if _, err := tx.ExecContext(ctx,
				fmt.Sprintf(`UPDATE %s SET %s WHERE id = ?`, q(table), strings.Join(set, ", ")),
				uVals...); err != nil {
				return nil, 0, 0, ChangeRange{}, true, fmt.Errorf("update %s: %w", table, err)
			}
			if len(fts) > 0 {
				if err := reindexFTSRow(ctx, tx, table, fts, matchID); err != nil {
					return nil, 0, 0, ChangeRange{}, true, err
				}
			}
		}
		ids = append(ids, matchID)
		updateIDs = append(updateIDs, matchID)
		updateOwners = append(updateOwners, matchOwner)
		updated++
	}

	if len(embFor) > 0 && persistMeta {
		if sc.EmbedSpace == "" && emb.Identity != "" {
			sc.EmbedSpace = emb.Identity
		}
		if sc.EmbedDim == 0 {
			for _, v := range embFor {
				sc.EmbedDim = len(v)
				break
			}
		}
		raw, err := json.Marshal(sc)
		if err != nil {
			return nil, 0, 0, ChangeRange{}, true, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE _dolmen_tables SET schema_json = ? WHERE name = ?`, string(raw), table); err != nil {
			return nil, 0, 0, ChangeRange{}, true, err
		}
	}

	if len(insertIDs) > 0 {
		rng, err := mintChanges(ctx, tx, table, ChangeInsert, insertIDs, sameOwner(stampOwner(sc, owner), len(insertIDs)))
		if err != nil {
			return nil, 0, 0, ChangeRange{}, true, err
		}
		changes = rng
	}
	if len(updateIDs) > 0 {
		rng, err := mintChanges(ctx, tx, table, ChangeUpdate, updateIDs, updateOwners)
		if err != nil {
			return nil, 0, 0, ChangeRange{}, true, err
		}
		if changes.Count == 0 {
			changes = rng
		} else {
			changes.Last = rng.Last
			changes.Count += rng.Count
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, 0, ChangeRange{}, true, err
	}

	s.notifyCommitted(nsName, table, changes)
	return ids, inserted, updated, changes, true, nil
}

func reindexFTSRow(ctx context.Context, tx *sql.Tx, table string, fts []schema.Field, id int64) error {
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE rowid = ?`, q(ftsTable(table))), id); err != nil {
		return fmt.Errorf("update search index for %s: %w", table, err)
	}
	cols := make([]string, len(fts))
	for i, f := range fts {
		cols[i] = q(f.Name)
	}
	joined := strings.Join(cols, ", ")
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO %s (rowid, %s) SELECT id, %s FROM %s WHERE id = ?`,
			q(ftsTable(table)), joined, joined, q(table)), id); err != nil {
		return fmt.Errorf("update search index for %s: %w", table, err)
	}
	return nil
}
