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
	"github.com/lsm/dolmen/internal/value"
)

func databaseValue(field schema.Field, input any) (any, error) {
	v, err := value.Coerce(field, input)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", store.ErrInvalid, err)
	}
	if v != nil {
		if field.Type == schema.Boolean {
			v = v.(int64) != 0
		} else if field.Type == schema.Number {
			v = storedNumber(v)
		}
	}
	return v, nil
}

func (s *Store) matchKey(ctx context.Context, tx pgx.Tx, n namespace, state tableState, keys []string, record map[string]any, index int, scope *store.RowScope) (int64, error) {
	predicates := make([]string, len(keys))
	args := make([]any, len(keys))
	for i, key := range keys {
		field := state.schema.Field(key)
		v, err := databaseValue(*field, record[key])
		if err != nil {
			return 0, err
		}
		if v == nil {
			return 0, fmt.Errorf("%w: record %d: key field %q must be present and non-null", store.ErrInvalid, index, key)
		}
		args[i] = v
		predicates[i] = ident(state.columns[key]) + "=$" + strconv.Itoa(i+1)
	}
	prefix, source, scopeArgs := scopedSourceAt(ident(n.physical, state.physical), scope, len(args)+1)
	args = append(args, scopeArgs...)
	rows, err := tx.Query(ctx, prefix+"SELECT id FROM "+source+" WHERE "+strings.Join(predicates, " AND ")+" ORDER BY id LIMIT 2", args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) > 1 {
		return 0, derr.New(derr.Conflict, "record %d: natural key (%s) matches multiple existing rows (ids %d and %d); delete duplicate rows before upserting", index, strings.Join(keys, ", "), ids[0], ids[1])
	}
	if len(ids) == 1 {
		return ids[0], nil
	}
	return 0, nil
}

func insertPrepared(ctx context.Context, tx pgx.Tx, n namespace, state tableState, row preparedRow) (int64, error) {
	params := make([]string, len(row.values))
	for i := range params {
		params[i] = "$" + strconv.Itoa(i+1)
	}
	var id int64
	err := tx.QueryRow(ctx, "INSERT INTO "+ident(n.physical, state.physical)+" ("+strings.Join(row.columns, ",")+") VALUES("+strings.Join(params, ",")+") RETURNING id", row.values...).Scan(&id)
	return id, err
}

func updatePrepared(ctx context.Context, tx pgx.Tx, n namespace, state tableState, row preparedRow, id int64) error {
	if len(row.columns) == 0 {
		return nil
	}
	assignments := make([]string, len(row.columns))
	for i, column := range row.columns {
		assignments[i] = column + "=$" + strconv.Itoa(i+1)
	}
	args := append(append([]any{}, row.values...), id)
	_, err := tx.Exec(ctx, "UPDATE "+ident(n.physical, state.physical)+" SET "+strings.Join(assignments, ",")+" WHERE id=$"+strconv.Itoa(len(args)), args...)
	return err
}

func (s *Store) UpsertByKey(ctx context.Context, ns, table string, keys []string, records []map[string]any, opts store.WriteOpts, emb store.Embedder, scope *store.RowScope, expected store.Incarnation) (store.InsertResult, error) {
	records, err := normalizeRecords(records)
	if err != nil {
		return store.InsertResult{}, err
	}
	keys, err = store.NormalizeKeyFields(keys)
	if err != nil {
		return store.InsertResult{}, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		var state tableState
		err := s.read(ctx, ns, func(tx pgx.Tx, n namespace) error {
			var err error
			state, err = s.loadTable(ctx, tx, n, table)
			if err != nil {
				return err
			}
			return s.guardScope(ctx, tx, n, table, state, expected)
		})
		if err != nil {
			return store.InsertResult{}, err
		}
		for _, key := range keys {
			field := state.schema.Field(key)
			if field == nil {
				return store.InsertResult{}, fmt.Errorf("%w: key field %q is not a field of table %s", store.ErrInvalid, key, table)
			}
			switch field.Type {
			case schema.String, schema.Text, schema.Number, schema.Boolean, schema.Timestamp:
			default:
				return store.InsertResult{}, fmt.Errorf("%w: key field %q has type %s; keys must be string, text, number, boolean, or timestamp (vector and json values do not compare reliably, and secret values are encrypted under a fresh nonce so equal values never match)", store.ErrInvalid, key, field.Type)
			}
			for i, record := range records {
				if record[key] == nil {
					return store.InsertResult{}, fmt.Errorf("%w: record %d: key field %q must be present and non-null", store.ErrInvalid, i, key)
				}
			}
		}
		before, _ := json.Marshal(state.schema)
		prepared, err := s.prepareValues(ctx, state, records, emb, false)
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
			if err := s.guardScope(ctx, tx, n, table, current, expected); err != nil {
				return err
			}
			if err := checkIncarnation(ns, current.incarnation, state.incarnation); err != nil {
				return err
			}
			if err := scopeUsable(scope, current.schema); err != nil {
				return err
			}
			now, _ := json.Marshal(current.schema)
			if string(now) != string(before) {
				retry = true
				return nil
			}
			inserted, updated := []int64{}, []int64{}
			stamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
			for i, row := range prepared {
				id, err := s.matchKey(ctx, tx, n, state, keys, records[i], i, scope)
				if err != nil {
					return err
				}
				if id == 0 {
					for _, field := range state.schema.Fields {
						if _, present := records[i][field.Name]; present {
							continue
						}
						if field.Required {
							return fmt.Errorf("%w: record %d: field %q is required because no existing row matched", store.ErrInvalid, i, field.Name)
						}
						if field.Default == nil {
							continue
						}
						defaultValue := field.Default
						if field.Type == schema.Timestamp && schema.IsNowDefault(defaultValue) {
							defaultValue = stamp
						}
						v, err := databaseValue(field, defaultValue)
						if err != nil {
							return err
						}
						row.columns = append(row.columns, ident(state.columns[field.Name]))
						row.values = append(row.values, v)
					}
					if opts.Owner != "" && state.schema.HasOwner {
						row.columns = append(row.columns, ident(schema.OwnerColumn))
						row.values = append(row.values, opts.Owner)
					}
					id, err = insertPrepared(ctx, tx, n, state, row)
					if err != nil {
						return err
					}
					inserted = append(inserted, id)
				} else {
					if err := updatePrepared(ctx, tx, n, state, row, id); err != nil {
						return err
					}
					updated = append(updated, id)
				}
				result.Ids = append(result.Ids, id)
			}
			after, err := json.Marshal(state.schema)
			if err != nil {
				return err
			}
			if string(after) != string(before) {
				if after, err = s.keepUnknownKeys(ctx, tx, ns, table, after, nil); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, "UPDATE "+s.relation("tables")+" SET schema_json=$1 WHERE namespace=$2 AND name=$3", string(after), ns, table); err != nil {
					return err
				}
			}
			first, err := s.mintChanges(ctx, tx, n, state, store.ChangeInsert, inserted, sameOwner(state.schema, opts.Owner, len(inserted)))
			if err != nil {
				return err
			}
			updatedOwners, err := s.ownersOf(ctx, tx, n, state, updated)
			if err != nil {
				return err
			}
			second, err := s.mintChanges(ctx, tx, n, state, store.ChangeUpdate, updated, updatedOwners)
			if err != nil {
				return err
			}
			result.Changes = first
			if first.Count == 0 {
				result.Changes = second
			} else if second.Count > 0 {
				result.Changes.Last = second.Last
				result.Changes.Count += second.Count
			}
			result.Inserted = int64(len(inserted))
			result.Updated = int64(len(updated))
			return nil
		})
		if err != nil {
			return store.InsertResult{}, err
		}
		if !retry {
			return result, nil
		}
	}
	return store.InsertResult{}, fmt.Errorf("%w: table schema changed concurrently; retry the upsert", store.ErrInvalid)
}
