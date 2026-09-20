package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func normalizeSet(set map[string]any) (map[string]any, error) {
	if len(set) == 0 {
		return nil, fmt.Errorf("%w: set is required (at least one field to update)", store.ErrInvalid)
	}
	normalized := make(map[string]any, len(set))
	for name, value := range set {
		key := strings.ToLower(name)
		if _, exists := normalized[key]; exists {
			return nil, fmt.Errorf("%w: fields %q and its case variant collapse to %q; use one spelling", store.ErrInvalid, name, key)
		}
		normalized[key] = value
	}
	return normalized, nil
}

func validateMutationSet(state tableState, set map[string]any) error {
	for name := range set {
		if state.schema.Field(name) == nil {
			return fmt.Errorf("%w: unknown field %q on table %s (see describe_table)", store.ErrInvalid, name, state.incarnation.Table)
		}
	}
	for _, field := range state.schema.Fields {
		value, present := set[field.Name]
		if !present {
			continue
		}
		if value == nil && field.Required {
			return fmt.Errorf("%w: field %q is required and cannot be set to null", store.ErrInvalid, field.Name)
		}
		if _, err := databaseValue(field, value); err != nil {
			return err
		}
	}
	return nil
}

func compileMutationFilter(filter string, argc int, physicalNamespace string, state tableState) (string, error) {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return "", fmt.Errorf("%w: filter is required (pass \"1=1\" to match every row)", store.ErrInvalid)
	}
	if strings.Contains(filter, ";") {
		return "", fmt.Errorf("%w: multiple statements are not allowed in filter", store.ErrInvalid)
	}
	query := "SELECT id FROM " + ident(state.incarnation.Table) + " WHERE " + filter + " ORDER BY id"
	compiled, _, err := compileSQL(query, argc, physicalNamespace, map[string]tableState{state.incarnation.Table: state})
	if err != nil {
		return "", filterSyntaxError(filter, err)
	}
	return compiled, nil
}

func filterSyntaxError(filter string, err error) error {
	if !strings.Contains(err.Error(), "invalid PostgreSQL SQL") {
		return err
	}
	return store.NewBackendQueryError(fmt.Sprintf("invalid filter %q: the filter must be a single SQL WHERE expression (e.g. \"status = 'done'\" or \"id IN (3, 7)\"); use ? for parameters and column names from describe_table", filter), err)
}

func selectMutationIDs(ctx context.Context, tx pgx.Tx, query string, args []any) ([]int64, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, queryError(ctx, err)
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, queryError(ctx, rows.Err())
}

func updateMatched(ctx context.Context, tx pgx.Tx, n namespace, state tableState, row preparedRow, ids []int64) error {
	if len(ids) == 0 || len(row.columns) == 0 {
		return nil
	}
	assignments := make([]string, len(row.columns))
	for i, column := range row.columns {
		assignments[i] = column + "=$" + strconv.Itoa(i+1)
	}
	args := append(append([]any{}, row.values...), ids)
	_, err := tx.Exec(ctx, "UPDATE "+ident(n.physical, state.physical)+" SET "+strings.Join(assignments, ",")+" WHERE id=ANY($"+strconv.Itoa(len(args))+"::bigint[])", args...)
	return queryError(ctx, err)
}

func addInsertDefaults(state tableState, record map[string]any, row preparedRow, stamp string) (preparedRow, error) {
	for _, field := range state.schema.Fields {
		if _, present := record[field.Name]; present {
			continue
		}
		if field.Default != nil {
			value := field.Default
			if field.Type == schema.Timestamp && schema.IsNowDefault(value) {
				value = stamp
			}
			coerced, err := databaseValue(field, value)
			if err != nil {
				return row, err
			}
			row.columns = append(row.columns, ident(state.columns[field.Name]))
			row.values = append(row.values, coerced)
			continue
		}
		if field.Required {
			return row, fmt.Errorf("%w: field %q is required (no row matched the filter, so upsert would insert a new record)", store.ErrInvalid, field.Name)
		}
	}
	return row, nil
}

func validateInsertFallback(state tableState, record map[string]any) error {
	for _, field := range state.schema.Fields {
		if _, present := record[field.Name]; present || field.Default != nil || !field.Required {
			continue
		}
		return fmt.Errorf("%w: field %q is required (no row matched the filter, so upsert would insert a new record)", store.ErrInvalid, field.Name)
	}
	return nil
}

func (s *Store) mutate(ctx context.Context, ns, table, filter string, args []any, set map[string]any, emb store.Embedder, allowInsert bool, scope *store.RowScope, expected store.Incarnation) (store.InsertResult, error) {
	if scope != nil {
		return store.InsertResult{}, derr.New(derr.Forbidden, "PostgreSQL row scopes are not implemented yet")
	}
	set, err := normalizeSet(set)
	if err != nil {
		return store.InsertResult{}, err
	}
	args, err = queryArgs(args)
	if err != nil {
		return store.InsertResult{}, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		var state tableState
		var compiled string
		matched := false
		err := s.read(ctx, ns, func(tx pgx.Tx, n namespace) error {
			var err error
			state, err = s.loadTable(ctx, tx, n, table)
			if err != nil {
				return err
			}
			if err := checkIncarnation(ns, state.incarnation, expected); err != nil {
				return err
			}
			if err := validateMutationSet(state, set); err != nil {
				return err
			}
			compiled, err = compileMutationFilter(filter, len(args), n.physical, state)
			if err != nil {
				return err
			}
			ids, err := selectMutationIDs(ctx, tx, compiled, args)
			matched = len(ids) > 0
			return err
		})
		if err != nil {
			return store.InsertResult{}, err
		}
		if !matched && !allowInsert {
			return store.InsertResult{}, nil
		}
		if !matched {
			if err := validateInsertFallback(state, set); err != nil {
				return store.InsertResult{}, err
			}
		}
		before, _ := json.Marshal(state.schema)
		prepared, err := prepareValues(ctx, state, []map[string]any{set}, emb, false)
		if err != nil {
			return store.InsertResult{}, err
		}
		retry := false
		result := store.InsertResult{Ids: []int64{}}
		err = s.write(ctx, ns, state.incarnation.NsGen, func(tx pgx.Tx, n namespace) error {
			current, err := s.loadTable(ctx, tx, n, table)
			if err != nil {
				return err
			}
			if err := checkIncarnation(ns, current.incarnation, state.incarnation); err != nil {
				return err
			}
			now, _ := json.Marshal(current.schema)
			if string(now) != string(before) {
				retry = true
				return nil
			}
			ids, err := selectMutationIDs(ctx, tx, compiled, args)
			if err != nil {
				return err
			}
			if len(ids) > 0 {
				if err := updateMatched(ctx, tx, n, state, prepared[0], ids); err != nil {
					return err
				}
				result.Ids = ids
				result.Updated = int64(len(ids))
				result.Changes, err = s.mintChanges(ctx, tx, n, state, store.ChangeUpdate, ids)
			} else if allowInsert {
				var row preparedRow
				row, err = addInsertDefaults(state, set, prepared[0], time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
				if err != nil {
					return err
				}
				var id int64
				id, err = insertPrepared(ctx, tx, n, state, row)
				if err != nil {
					return queryError(ctx, err)
				}
				result.Ids = []int64{id}
				result.Inserted = 1
				result.Changes, err = s.mintChanges(ctx, tx, n, state, store.ChangeInsert, result.Ids)
			}
			if err != nil {
				return err
			}
			if result.Inserted+result.Updated > 0 {
				after, err := json.Marshal(state.schema)
				if err != nil {
					return err
				}
				if string(after) != string(before) {
					_, err = tx.Exec(ctx, "UPDATE "+s.relation("tables")+" SET schema_json=$1 WHERE namespace=$2 AND name=$3", string(after), ns, table)
					return err
				}
			}
			return nil
		})
		if err != nil {
			return store.InsertResult{}, err
		}
		if !retry {
			return result, nil
		}
	}
	return store.InsertResult{}, fmt.Errorf("%w: table schema changed concurrently; retry the mutation", store.ErrInvalid)
}

func (s *Store) Update(ctx context.Context, ns, table, filter string, args []any, set map[string]any, emb store.Embedder, scope *store.RowScope, expected store.Incarnation) (store.UpdateResult, error) {
	result, err := s.mutate(ctx, ns, table, filter, args, set, emb, false, scope, expected)
	return store.UpdateResult{Updated: result.Updated, Changes: result.Changes}, err
}

func (s *Store) Upsert(ctx context.Context, ns, table, filter string, args []any, set map[string]any, opts store.WriteOpts, emb store.Embedder, scope *store.RowScope, expected store.Incarnation) (store.InsertResult, error) {
	if opts.Owner != "" || opts.TableWideRead {
		return store.InsertResult{}, derr.New(derr.Forbidden, "PostgreSQL row authorization is not implemented yet")
	}
	return s.mutate(ctx, ns, table, filter, args, set, emb, true, scope, expected)
}

func (s *Store) Delete(ctx context.Context, ns, table, filter string, args []any, opts store.DeleteOpts, scope *store.RowScope, expected store.Incarnation) (store.DeleteResult, error) {
	if scope != nil {
		return store.DeleteResult{}, derr.New(derr.Forbidden, "PostgreSQL row scopes are not implemented yet")
	}
	args, err := queryArgs(args)
	if err != nil {
		return store.DeleteResult{}, err
	}
	if opts.DryRun {
		result := store.DeleteResult{}
		err := s.read(ctx, ns, func(tx pgx.Tx, n namespace) error {
			state, err := s.loadTable(ctx, tx, n, table)
			if err != nil {
				return err
			}
			if err := checkIncarnation(ns, state.incarnation, expected); err != nil {
				return err
			}
			compiled, err := compileMutationFilter(filter, len(args), n.physical, state)
			if err != nil {
				return err
			}
			ids, err := selectMutationIDs(ctx, tx, compiled, args)
			result.Matched = int64(len(ids))
			return err
		})
		return result, err
	}
	result := store.DeleteResult{}
	err = s.write(ctx, ns, expected.NsGen, func(tx pgx.Tx, n namespace) error {
		state, err := s.loadTable(ctx, tx, n, table)
		if err != nil {
			return err
		}
		if err := checkIncarnation(ns, state.incarnation, expected); err != nil {
			return err
		}
		compiled, err := compileMutationFilter(filter, len(args), n.physical, state)
		if err != nil {
			return err
		}
		ids, err := selectMutationIDs(ctx, tx, compiled, args)
		if err != nil {
			return err
		}
		result.Matched = int64(len(ids))
		limit := int64(store.DefaultDeleteLimit)
		if opts.Limit > 0 {
			limit = int64(opts.Limit)
		}
		if result.Matched > limit && !opts.Confirm {
			return fmt.Errorf("%w: filter matched %d rows, exceeding the delete limit of %d; pass confirm: true to proceed or dry_run: true to preview", store.ErrInvalid, result.Matched, limit)
		}
		if len(ids) == 0 {
			return nil
		}
		command, err := tx.Exec(ctx, "DELETE FROM "+ident(n.physical, state.physical)+" WHERE id=ANY($1::bigint[])", ids)
		if err != nil {
			return queryError(ctx, err)
		}
		result.Deleted = command.RowsAffected()
		result.Changes, err = s.mintChanges(ctx, tx, n, state, store.ChangeDelete, ids)
		return err
	})
	return result, err
}
