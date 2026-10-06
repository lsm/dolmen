package lakehouse

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/gob"
	"fmt"
	"maps"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
	"github.com/lsm/dolmen/internal/value"
)

func checkFilter(filter string) (string, error) {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return "", invalidf("filter is required (pass \"1=1\" to match everything)")
	}
	if strings.Contains(filter, ";") {
		return "", invalidf("multiple statements are not allowed in filter")
	}
	return filter, nil
}

func (s *Store) matchIDs(ctx context.Context, n *namespace, ns string, state tableState, filter string, args []any, scope *store.RowScope) ([]int64, error) {
	if scope != nil && scope.Empty {
		return nil, nil
	}
	sql := "SELECT " + quoteIdent("id") + " FROM " + viewName(state.schema.Name, true) + " WHERE (" + filter + ")"
	bound := append([]any{}, args...)
	if scope != nil && scope.Owner != "" && state.schema.HasOwner {
		sql += " AND " + quoteIdent(schema.OwnerColumn) + " = ?"
		bound = append(bound, scope.Owner)
	}
	sql += " ORDER BY " + quoteIdent("id")
	encoded := make([]string, len(bound))
	for i, a := range bound {
		var err error
		if encoded[i], err = queryArg(a); err != nil {
			return nil, err
		}
	}
	sc, _, err := s.ensureSidecar(ctx, n, ns)
	if err != nil {
		return nil, err
	}
	sc.run.Lock()
	fields, err := sc.call(ctx, "query", append([]string{"0", strconv.Itoa(math.MaxInt32), "0", sql}, encoded...)...)
	sc.run.Unlock()
	if err != nil {
		return nil, err
	}
	res, err := parseQueryReply(fields)
	if err != nil {
		return nil, store.NewFilterError(filter, err)
	}
	ids := make([]int64, 0, len(res.Rows))
	for _, row := range res.Rows {
		id, ok := row["id"].(int64)
		if !ok {
			return nil, fmt.Errorf("lakehouse filter returned a non-integer id %v", row["id"])
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func idSet(ids []int64) map[int64]bool {
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func (s *Store) currentRows(ctx context.Context, state tableState, ids []int64) (map[int64]map[string]any, error) {
	out := map[int64]map[string]any{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := scanRows(ctx, state, idFilter(ids))
	if err != nil {
		return nil, err
	}
	want := idSet(ids)
	for _, row := range rows {
		id := row["id"].(int64)
		if want[id] {
			out[id] = row
		}
	}
	return out, nil
}

func (s *Store) prepareSet(ctx context.Context, state tableState, set map[string]any, emb store.Embedder) (map[string]any, []string, error) {
	sc := state.schema
	values := map[string]any{}
	var cleared []string
	vf := sc.VectorizeField()
	for name, v := range set {
		f := sc.Field(name)
		if f == nil {
			return nil, nil, invalidf("unknown field %q on table %s (see describe_table)", name, sc.Name)
		}
		if v == nil {
			if f.Required {
				return nil, nil, invalidf("field %q is required", f.Name)
			}
			cleared = append(cleared, f.Name)
			continue
		}
		coerced, err := s.coerceField(*f, v, "")
		if err != nil {
			return nil, nil, err
		}
		values[f.Name] = coerced
	}
	if vf != nil {
		if _, touched := set[vf.Name]; touched {
			text, _ := values[vf.Name].(string)
			if text == "" {
				values["_embedding"] = nil
			} else {
				space, dim := sc.EmbedSpace, sc.EmbedDim
				vecs, err := store.EmbedTexts(ctx, sc, sc.Name, []string{text}, emb)
				if err != nil {
					return nil, nil, err
				}
				values["_embedding"] = schema.EncodeVector(vecs[0])
				if sc.EmbedDim != dim || space == "" {
					sc.EmbedSpace = emb.Identity
				}
			}
		}
	}
	return values, cleared, nil
}

func (s *Store) coerceField(f schema.Field, v any, stamp string) (any, error) {
	if f.Type == schema.Timestamp && schema.IsNowDefault(v) {
		v = stamp
	}
	coerced, err := value.Coerce(f, v)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", store.ErrInvalid, err)
	}
	switch f.Type {
	case schema.Boolean:
		coerced = coerced.(int64) != 0
	case schema.Number:
		coerced = canonicalNumber(coerced)
	case schema.Secret:
		return store.SealSecret(s.secrets, f, coerced)
	}
	return coerced, nil
}

type mutation struct {
	kind    store.ChangeKind
	deletes []int64
	updates map[int64]map[string]any
	inserts []map[string]any
	owners  map[int64]string
	nextID  int64
}

func (s *Store) commitMutation(ctx context.Context, n *namespace, state tableState, m mutation, stamp string) (store.ChangeRange, error) {
	tx, err := n.db.BeginTx(ctx, nil)
	if err != nil {
		return store.ChangeRange{}, err
	}
	defer tx.Rollback()
	inc := state.incarnation
	if m.nextID > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO _dolmen_lakehouse_ids(table_name, generation, next_id) VALUES(?,?,?) ON CONFLICT(table_name, generation) DO UPDATE SET next_id=excluded.next_id`, inc.Table, inc.DropGen, m.nextID); err != nil {
			return store.ChangeRange{}, err
		}
	}
	payload := commitRows{Delete: append(append([]int64{}, m.deletes...), mapKeys(m.updates)...)}
	for _, row := range m.inserts {
		payload.Rows = append(payload.Rows, row)
	}
	for _, id := range sortedKeys(m.updates) {
		payload.Rows = append(payload.Rows, m.updates[id])
	}
	for _, row := range payload.Rows {
		id := row["id"].(int64)
		for _, f := range state.schema.SecretFields() {
			v, present := row[f.Name]
			if !present {
				continue
			}
			sealed, isSealed := v.([]byte)
			if isSealed && !bytes.Equal(sealed, secretPresent) {
				if _, err := tx.ExecContext(ctx, `INSERT INTO _dolmen_lakehouse_secrets(table_name, generation, row_id, field, value) VALUES(?,?,?,?,?) ON CONFLICT(table_name, generation, row_id, field) DO UPDATE SET value = excluded.value`, inc.Table, inc.DropGen, id, f.Name, sealed); err != nil {
					return store.ChangeRange{}, err
				}
				row[f.Name] = secretPresent
			} else if v == nil {
				if _, err := tx.ExecContext(ctx, `DELETE FROM _dolmen_lakehouse_secrets WHERE table_name=? AND generation=? AND row_id=? AND field=?`, inc.Table, inc.DropGen, id, f.Name); err != nil {
					return store.ChangeRange{}, err
				}
			}
		}
	}
	for _, id := range m.deletes {
		if _, err := tx.ExecContext(ctx, `DELETE FROM _dolmen_lakehouse_secrets WHERE table_name=? AND generation=? AND row_id=?`, inc.Table, inc.DropGen, id); err != nil {
			return store.ChangeRange{}, err
		}
	}
	for _, row := range payload.Rows {
		for k, v := range row {
			if v == nil {
				delete(row, k)
			}
		}
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(payload); err != nil {
		return store.ChangeRange{}, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO _dolmen_lakehouse_commits(table_name, generation, kind, rows) VALUES(?,?,?,?)`, inc.Table, inc.DropGen, string(m.kind), buf.Bytes())
	if err != nil {
		return store.ChangeRange{}, err
	}
	commit, err := res.LastInsertId()
	if err != nil {
		return store.ChangeRange{}, err
	}
	var changes store.ChangeRange
	mint := func(id int64, kind store.ChangeKind) error {
		var owner any
		if state.schema.HasOwner {
			owner = m.owners[id]
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO _dolmen_lakehouse_changes(table_name, generation, row_id, kind, owner, commit_id, at) VALUES(?,?,?,?,?,?,?)`, inc.Table, inc.DropGen, id, string(kind), owner, commit, stamp)
		if err != nil {
			return err
		}
		seq, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if changes.Count == 0 {
			changes.First = seq
		}
		changes.Last = seq
		changes.Count++
		return nil
	}
	countDelta := map[string]int64{}
	for _, row := range m.inserts {
		id := row["id"].(int64)
		if err := mint(id, store.ChangeInsert); err != nil {
			return store.ChangeRange{}, err
		}
		owner := ""
		if state.schema.HasOwner {
			owner = m.owners[id]
		}
		countDelta[owner]++
	}
	for _, id := range sortedKeys(m.updates) {
		if err := mint(id, store.ChangeUpdate); err != nil {
			return store.ChangeRange{}, err
		}
	}
	for _, id := range m.deletes {
		if err := mint(id, store.ChangeDelete); err != nil {
			return store.ChangeRange{}, err
		}
		owner := ""
		if state.schema.HasOwner {
			owner = m.owners[id]
		}
		countDelta[owner]--
	}
	for owner, delta := range countDelta {
		if delta == 0 {
			continue
		}
		stmt := `INSERT INTO _dolmen_lakehouse_counts(table_name, generation, owner, n) VALUES(?,?,?,?) ON CONFLICT(table_name, generation, owner) DO UPDATE SET n = n + excluded.n`
		args := []any{inc.Table, inc.DropGen, owner, delta}
		if delta < 0 {
			stmt = `UPDATE _dolmen_lakehouse_counts SET n = n + ? WHERE table_name=? AND generation=? AND owner=?`
			args = []any{delta, inc.Table, inc.DropGen, owner}
		}
		if _, err := tx.ExecContext(ctx, stmt, args...); err != nil {
			return store.ChangeRange{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return store.ChangeRange{}, err
	}
	n.pending++
	return changes, nil
}

func mapKeys(m map[int64]map[string]any) []int64 { return sortedKeys(m) }

func sortedKeys(m map[int64]map[string]any) []int64 {
	keys := make([]int64, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func ownersOf(state tableState, rows map[int64]map[string]any) map[int64]string {
	out := map[int64]string{}
	if !state.schema.HasOwner {
		return out
	}
	for id, row := range rows {
		owner, _ := row[schema.OwnerColumn].(string)
		out[id] = owner
	}
	return out
}

func (s *Store) Delete(ctx context.Context, ns, name, filter string, args []any, opts store.DeleteOpts, scope *store.RowScope, scopeIncarnation store.Incarnation) (store.DeleteResult, error) {
	filter, err := store.NormalizeDeleteFilter(filter, args)
	if err != nil {
		return store.DeleteResult{}, err
	}
	var result store.DeleteResult
	err = s.withNamespace(ctx, ns, func(n *namespace) error {
		state, err := loadTable(ctx, n, ns, name)
		if err != nil {
			return err
		}
		if err := checkExpected(state, scopeIncarnation, false); err != nil {
			return err
		}
		if scope != nil && !scope.Empty && !state.schema.HasOwner {
			return invalidf("table %s carries no owner column, so a row scope cannot be applied to it", name)
		}
		ids, err := s.matchIDs(ctx, n, ns, state, filter, args, scope)
		if err != nil {
			return err
		}
		result.Matched = int64(len(ids))
		if opts.DryRun {
			return nil
		}
		limit := int64(store.DefaultDeleteLimit)
		if opts.Limit > 0 {
			limit = int64(opts.Limit)
		}
		if result.Matched > limit && !opts.Confirm {
			advice := "pass confirm: true to proceed or dry_run: true to preview"
			if opts.NoDryRunAdvice {
				advice = "pass confirm: true to proceed"
			}
			return invalidf("filter matched %d rows, exceeding the delete limit of %d; %s", result.Matched, limit, advice)
		}
		if len(ids) == 0 {
			return nil
		}
		rows, err := s.currentRows(ctx, state, ids)
		if err != nil {
			return err
		}
		stamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
		result.Changes, err = s.commitMutation(ctx, n, state, mutation{kind: store.ChangeDelete, deletes: ids, owners: ownersOf(state, rows)}, stamp)
		if err != nil {
			return err
		}
		result.Deleted = result.Matched
		s.materialize(ctx, n, ns)
		return nil
	})
	return result, err
}

func (s *Store) Update(ctx context.Context, ns, name, filter string, args []any, set map[string]any, emb store.Embedder, scope *store.RowScope, scopeIncarnation store.Incarnation) (store.UpdateResult, error) {
	res, err := s.mutate(ctx, ns, name, filter, args, set, store.WriteOpts{}, emb, false, scope, scopeIncarnation)
	return store.UpdateResult{Updated: res.Updated, Changes: res.Changes}, err
}

func (s *Store) Upsert(ctx context.Context, ns, name, filter string, args []any, record map[string]any, opts store.WriteOpts, emb store.Embedder, scope *store.RowScope, scopeIncarnation store.Incarnation) (store.InsertResult, error) {
	return s.mutate(ctx, ns, name, filter, args, record, opts, emb, true, scope, scopeIncarnation)
}

func lowerKeys(m map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(m))
	for k, v := range m {
		key := strings.ToLower(k)
		if _, dup := out[key]; dup {
			return nil, invalidf("fields %q and its case variant collapse to %q; use one spelling", k, key)
		}
		out[key] = v
	}
	return out, nil
}

func (s *Store) mutate(ctx context.Context, ns, name, filter string, args []any, set map[string]any, opts store.WriteOpts, emb store.Embedder, allowInsert bool, scope *store.RowScope, scopeIncarnation store.Incarnation) (store.InsertResult, error) {
	filter, err := checkFilter(filter)
	if err != nil {
		return store.InsertResult{}, err
	}
	if len(set) == 0 {
		return store.InsertResult{}, invalidf("set is required: name at least one field to change")
	}
	set, err = lowerKeys(set)
	if err != nil {
		return store.InsertResult{}, err
	}
	var result store.InsertResult
	err = s.withNamespace(ctx, ns, func(n *namespace) error {
		state, err := loadTable(ctx, n, ns, name)
		if err != nil {
			return err
		}
		if err := checkExpected(state, scopeIncarnation, false); err != nil {
			return err
		}
		if scope != nil && !scope.Empty && !state.schema.HasOwner {
			return invalidf("table %s carries no owner column, so a row scope cannot be applied to it", name)
		}
		if err := store.RequireSecretKey(s.secrets, state.schema.Fields); err != nil {
			return err
		}
		ids, err := s.matchIDs(ctx, n, ns, state, filter, args, scope)
		if err != nil {
			return err
		}
		stamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
		space, dim := state.schema.EmbedSpace, state.schema.EmbedDim
		if len(ids) == 0 {
			if !allowInsert {
				return nil
			}
			rows, err := s.prepareRows(ctx, state, []map[string]any{set}, emb, stamp, opts.Owner)
			if err != nil {
				return err
			}
			if err := s.pinEmbedding(ctx, n, state, space, dim, emb); err != nil {
				return err
			}
			tx, err := n.db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			committed, err := s.commitAppend(ctx, tx, state, rows, store.WriteOpts{Owner: opts.Owner}, store.DomainFor(opts, scope), store.IdemHash{}, stamp, commitRows{})
			if err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			n.pending++
			result = committed
			result.Inserted = 1
			s.materialize(ctx, n, ns)
			return nil
		}
		values, cleared, err := s.prepareSet(ctx, state, set, emb)
		if err != nil {
			return err
		}
		if err := s.pinEmbedding(ctx, n, state, space, dim, emb); err != nil {
			return err
		}
		current, err := s.currentRows(ctx, state, ids)
		if err != nil {
			return err
		}
		updates := map[int64]map[string]any{}
		for _, id := range ids {
			old, ok := current[id]
			if !ok {
				continue
			}
			next := maps.Clone(old)
			maps.Copy(next, values)
			for _, field := range cleared {
				next[field] = nil
			}
			updates[id] = next
		}
		result.Changes, err = s.commitMutation(ctx, n, state, mutation{kind: store.ChangeUpdate, updates: updates, owners: ownersOf(state, current)}, stamp)
		if err != nil {
			return err
		}
		result.Ids = sortedKeys(updates)
		result.Updated = int64(len(updates))
		s.materialize(ctx, n, ns)
		return nil
	})
	return result, err
}

func (s *Store) pinEmbedding(ctx context.Context, n *namespace, state tableState, space string, dim int, emb store.Embedder) error {
	if state.schema.EmbedDim != dim || space == "" && state.schema.EmbedDim != 0 {
		state.schema.EmbedSpace = emb.Identity
		return s.publishSchema(ctx, n, state)
	}
	return nil
}

func (s *Store) UpsertByKey(ctx context.Context, ns, name string, on []string, records []map[string]any, opts store.WriteOpts, emb store.Embedder, scope *store.RowScope, scopeIncarnation store.Incarnation) (store.InsertResult, error) {
	keys, err := store.NormalizeKeyFields(on)
	if err != nil {
		return store.InsertResult{}, err
	}
	records, err = normalizeRecords(records)
	if err != nil {
		return store.InsertResult{}, err
	}
	var result store.InsertResult
	err = s.withNamespace(ctx, ns, func(n *namespace) error {
		state, err := loadTable(ctx, n, ns, name)
		if err != nil {
			return err
		}
		if err := checkExpected(state, scopeIncarnation, false); err != nil {
			return err
		}
		if scope != nil && !scope.Empty && !state.schema.HasOwner {
			return invalidf("table %s carries no owner column, so a row scope cannot be applied to it", name)
		}
		if err := store.RequireSecretKey(s.secrets, state.schema.Fields); err != nil {
			return err
		}
		for _, k := range keys {
			f := state.schema.Field(k)
			if f == nil {
				return invalidf("key field %q is not a field of table %s (see describe_table)", k, name)
			}
			switch f.Type {
			case schema.String, schema.Text, schema.Number, schema.Boolean, schema.Timestamp:
			default:
				return invalidf("key field %q has type %s; natural keys must be string, text, number, boolean, or timestamp fields (vector and json values do not compare reliably, and secret values are encrypted under a fresh nonce so equal values never match)", k, f.Type)
			}
		}
		stamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
		space, dim := state.schema.EmbedSpace, state.schema.EmbedDim
		var inserts []map[string]any
		updates := map[int64]map[string]any{}
		var current map[int64]map[string]any
		var allIDs []int64
		pending := map[string]int64{}
		var next int64
		if err := n.db.QueryRowContext(ctx, `SELECT next_id FROM _dolmen_lakehouse_ids WHERE table_name=? AND generation=?`, state.incarnation.Table, state.incarnation.DropGen).Scan(&next); err == sql.ErrNoRows {
			next = 1
		} else if err != nil {
			return err
		}
		owners := map[int64]string{}
		for i, rec := range records {
			args := make([]any, len(keys))
			conds := make([]string, len(keys))
			keyParts := make([]string, len(keys))
			for j, k := range keys {
				v, ok := rec[k]
				if !ok || v == nil {
					return invalidf("records[%d]: key field %q is required", i, k)
				}
				f := state.schema.Field(k)
				coerced, err := s.coerceField(*f, v, stamp)
				if err != nil {
					return err
				}
				args[j] = coerced
				if f.Type == schema.Number {
					args[j] = v
				}
				conds[j] = quoteIdent(k) + " = ?"
				keyParts[j] = fmt.Sprint(coerced)
			}
			signature := strings.Join(keyParts, "\x1f")
			if id, ok := pending[signature]; ok {
				row := findRow(inserts, updates, id)
				values, cleared, err := s.prepareSet(ctx, state, rec, emb)
				if err != nil {
					return err
				}
				maps.Copy(row, values)
				for _, field := range cleared {
					row[field] = nil
				}
				allIDs = append(allIDs, id)
				result.Updated++
				continue
			}
			ids, err := s.matchIDs(ctx, n, ns, state, strings.Join(conds, " AND "), args, scope)
			if err != nil {
				return err
			}
			if len(ids) == 0 {
				rows, err := s.prepareRows(ctx, state, []map[string]any{rec}, emb, stamp, opts.Owner)
				if err != nil {
					return fmt.Errorf("records[%d]: %w", i, err)
				}
				rows[0]["id"] = next
				owners[next] = opts.Owner
				inserts = append(inserts, rows[0])
				pending[signature] = next
				allIDs = append(allIDs, next)
				next++
				result.Inserted++
				continue
			}
			if len(ids) > 1 {
				return derr.New(derr.Conflict, "record %d: natural key (%s) matches multiple existing rows (ids %d and %d); the key is not unique in the table — delete the duplicate rows before upserting", i, strings.Join(keys, ", "), ids[0], ids[1])
			}
			id := ids[0]
			if current == nil {
				current = map[int64]map[string]any{}
			}
			if _, ok := current[id]; !ok {
				rows, err := s.currentRows(ctx, state, []int64{id})
				if err != nil {
					return err
				}
				maps.Copy(current, rows)
			}
			old, ok := current[id]
			if !ok {
				return fmt.Errorf("lakehouse row %d matched its key but could not be read", id)
			}
			values, cleared, err := s.prepareSet(ctx, state, rec, emb)
			if err != nil {
				return fmt.Errorf("records[%d]: %w", i, err)
			}
			row := maps.Clone(old)
			maps.Copy(row, values)
			for _, field := range cleared {
				row[field] = nil
			}
			updates[id] = row
			pending[signature] = id
			owner, _ := old[schema.OwnerColumn].(string)
			owners[id] = owner
			allIDs = append(allIDs, id)
			result.Updated++
		}
		if err := s.pinEmbedding(ctx, n, state, space, dim, emb); err != nil {
			return err
		}
		var nextID int64
		if len(inserts) > 0 {
			nextID = next
			for _, row := range inserts {
				row["created_at"] = stamp
				if state.schema.HasOwner {
					row[schema.OwnerColumn] = opts.Owner
				}
			}
		}
		result.Changes, err = s.commitMutation(ctx, n, state, mutation{kind: store.ChangeUpdate, inserts: inserts, updates: updates, owners: owners, nextID: nextID}, stamp)
		if err != nil {
			return err
		}
		result.Ids = allIDs
		s.materialize(ctx, n, ns)
		return nil
	})
	return result, err
}

func findRow(inserts []map[string]any, updates map[int64]map[string]any, id int64) map[string]any {
	if row, ok := updates[id]; ok {
		return row
	}
	for _, row := range inserts {
		if row["id"].(int64) == id {
			return row
		}
	}
	return nil
}
