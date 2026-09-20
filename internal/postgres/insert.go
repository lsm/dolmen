package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

type preparedRow struct {
	columns []string
	values  []any
}

type currentTimestamp struct{}

func normalizeRecords(records []map[string]any) ([]map[string]any, error) {
	if len(records) == 0 {
		return nil, fmt.Errorf("%w: no records given", store.ErrInvalid)
	}
	if len(records) > store.MaxRecordsPerInsert {
		return nil, fmt.Errorf("%w: too many records: %d > %d per call", store.ErrInvalid, len(records), store.MaxRecordsPerInsert)
	}
	out := make([]map[string]any, len(records))
	for i, rec := range records {
		out[i] = map[string]any{}
		for k, v := range rec {
			key := strings.ToLower(k)
			if _, exists := out[i][key]; exists {
				return nil, fmt.Errorf("%w: record %d: fields %q and its case variant collapse to %q; use one spelling", store.ErrInvalid, i, k, key)
			}
			out[i][key] = v
		}
	}
	return out, nil
}

func recordHash(records []map[string]any) (string, error) {
	raw, err := json.Marshal(records)
	if err != nil {
		return "", fmt.Errorf("%w: cannot encode records: %v", store.ErrInvalid, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (s *Store) lookupIdempotency(ctx context.Context, tx pgx.Tx, n namespace, state tableState, key, hash string) (store.InsertResult, bool, error) {
	var result store.InsertResult
	if key == "" {
		return result, false, nil
	}
	var stored, raw string
	err := tx.QueryRow(ctx, "SELECT payload_hash,result_json FROM "+s.relation("idempotency")+" WHERE namespace=$1 AND table_name=$2 AND drop_generation=$3 AND key=$4", n.name, state.incarnation.Table, state.incarnation.DropGen, key).Scan(&stored, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, false, nil
	}
	if err != nil {
		return result, false, err
	}
	if stored != hash {
		return result, false, derr.New(derr.Conflict, "idempotency key %q was already recorded for a different insert into %s; re-send the identical body for a retry, or use a fresh key for a new insert", key, state.incarnation.Table)
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return result, false, fmt.Errorf("postgres: corrupt idempotency record: %w", err)
	}
	result.Replayed = true
	result.Changes = store.ChangeRange{}
	return result, true, nil
}

func prepareRows(ctx context.Context, state tableState, records []map[string]any, emb store.Embedder) ([]preparedRow, error) {
	return prepareValues(ctx, state, records, emb, true)
}

func prepareValues(ctx context.Context, state tableState, records []map[string]any, emb store.Embedder, insert bool) ([]preparedRow, error) {
	out := make([]preparedRow, len(records))
	texts := []string{}
	indices := []int{}
	vf := state.schema.VectorizeField()
	for i, rec := range records {
		for name := range rec {
			if state.schema.Field(name) == nil {
				return nil, fmt.Errorf("%w: unknown field %q on table %s (see describe_table)", store.ErrInvalid, name, state.incarnation.Table)
			}
		}
		for _, f := range state.schema.Fields {
			v, present := rec[f.Name]
			if !present && !insert {
				continue
			}
			if !present && f.Default != nil {
				v = f.Default
				if f.Type == schema.Timestamp && schema.IsNowDefault(v) {
					v = currentTimestamp{}
				}
			}
			if v == nil && f.Required {
				return nil, fmt.Errorf("%w: field %q is required", store.ErrInvalid, f.Name)
			}
			var coerced any
			if _, ok := v.(currentTimestamp); ok {
				coerced = v
			} else {
				var err error
				coerced, err = value.Coerce(f, v)
				if err != nil {
					return nil, fmt.Errorf("%w: %w", store.ErrInvalid, err)
				}
			}
			if f.Type == schema.Boolean && coerced != nil {
				coerced = coerced.(int64) != 0
			}
			if f.Type == schema.Number && coerced != nil {
				coerced = fmt.Sprint(coerced)
			}
			out[i].columns = append(out[i].columns, ident(state.columns[f.Name]))
			out[i].values = append(out[i].values, coerced)
			if vf != nil && f.Name == vf.Name {
				if text, ok := coerced.(string); ok && text != "" {
					texts = append(texts, text)
					indices = append(indices, i)
				} else if !insert {
					out[i].columns = append(out[i].columns, ident("_embedding"))
					out[i].values = append(out[i].values, nil)
				}
			}
		}
	}
	if len(texts) > 0 {
		vecs, err := store.EmbedTexts(ctx, state.schema, state.incarnation.Table, texts, emb)
		if err != nil {
			return nil, err
		}
		state.schema.EmbedSpace = emb.Identity
		for j, i := range indices {
			out[i].columns = append(out[i].columns, ident("_embedding"))
			out[i].values = append(out[i].values, schema.EncodeVector(vecs[j]))
		}
	}
	return out, nil
}

func (s *Store) mintChanges(ctx context.Context, tx pgx.Tx, n namespace, state tableState, kind store.ChangeKind, ids []int64) (store.ChangeRange, error) {
	if len(ids) == 0 {
		return store.ChangeRange{}, nil
	}
	change, err := s.reserveChanges(ctx, tx, n, int64(len(ids)))
	if err != nil {
		return change, err
	}
	for i, id := range ids {
		if _, err := tx.Exec(ctx, "INSERT INTO "+s.relation("changes")+" (namespace,position,table_name,drop_generation,row_id,kind) VALUES($1,$2,$3,$4,$5,$6)", n.name, change.First+int64(i), state.incarnation.Table, state.incarnation.DropGen, id, string(kind)); err != nil {
			return store.ChangeRange{}, err
		}
	}
	if err := s.announce(ctx, tx, n.name); err != nil {
		return store.ChangeRange{}, err
	}
	return change, nil
}

func (s *Store) Insert(ctx context.Context, ns, table string, records []map[string]any, opts store.WriteOpts, emb store.Embedder, scope *store.RowScope, expected store.Incarnation) (store.InsertResult, error) {
	if scope != nil || opts.Owner != "" || opts.TableWideRead {
		return store.InsertResult{}, derr.New(derr.Forbidden, "PostgreSQL row authorization is not implemented yet")
	}
	if len(opts.IdempotencyKey) > store.MaxIdempotencyKeyLen {
		return store.InsertResult{}, fmt.Errorf("%w: idempotency key is %d bytes (max %d)", store.ErrInvalid, len(opts.IdempotencyKey), store.MaxIdempotencyKeyLen)
	}
	records, err := normalizeRecords(records)
	if err != nil {
		return store.InsertResult{}, err
	}
	hash := ""
	if opts.IdempotencyKey != "" {
		hash, err = recordHash(records)
		if err != nil {
			return store.InsertResult{}, err
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		var state tableState
		var result store.InsertResult
		found := false
		err := s.read(ctx, ns, func(tx pgx.Tx, n namespace) error {
			var err error
			state, err = s.loadTable(ctx, tx, n, table)
			if err != nil {
				return err
			}
			if err := checkIncarnation(ns, state.incarnation, expected); err != nil {
				return err
			}
			result, found, err = s.lookupIdempotency(ctx, tx, n, state, opts.IdempotencyKey, hash)
			return err
		})
		if err != nil || found {
			return result, err
		}
		before, _ := json.Marshal(state.schema)
		rows, err := prepareRows(ctx, state, records, emb)
		if err != nil {
			return store.InsertResult{}, err
		}
		retry := false
		err = s.write(ctx, ns, state.incarnation.NsGen, func(tx pgx.Tx, n namespace) error {
			current, err := s.loadTable(ctx, tx, n, table)
			if err != nil {
				return err
			}
			if err := checkIncarnation(ns, current.incarnation, state.incarnation); err != nil {
				return err
			}
			result, found, err = s.lookupIdempotency(ctx, tx, n, current, opts.IdempotencyKey, hash)
			if err != nil || found {
				return err
			}
			now, _ := json.Marshal(current.schema)
			if string(now) != string(before) {
				retry = true
				return nil
			}
			result.Ids = make([]int64, 0, len(rows))
			stamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
			for _, row := range rows {
				params := make([]string, len(row.values))
				for i, v := range row.values {
					params[i] = "$" + strconv.Itoa(i+1)
					if _, ok := v.(currentTimestamp); ok {
						row.values[i] = stamp
					}
				}
				var id int64
				stmt := "INSERT INTO " + ident(n.physical, state.physical) + " (" + strings.Join(row.columns, ",") + ") VALUES (" + strings.Join(params, ",") + ") RETURNING id"
				if err := tx.QueryRow(ctx, stmt, row.values...).Scan(&id); err != nil {
					return err
				}
				result.Ids = append(result.Ids, id)
			}
			after, err := json.Marshal(state.schema)
			if err != nil {
				return err
			}
			if string(after) != string(before) {
				if _, err := tx.Exec(ctx, "UPDATE "+s.relation("tables")+" SET schema_json=$1 WHERE namespace=$2 AND name=$3", string(after), ns, table); err != nil {
					return err
				}
			}
			result.Changes, err = s.mintChanges(ctx, tx, n, state, store.ChangeInsert, result.Ids)
			if err != nil {
				return err
			}
			if opts.IdempotencyKey != "" {
				raw, err := json.Marshal(result)
				if err != nil {
					return err
				}
				_, err = tx.Exec(ctx, "INSERT INTO "+s.relation("idempotency")+" (namespace,table_name,drop_generation,key,payload_hash,result_json) VALUES($1,$2,$3,$4,$5,$6)", ns, table, state.incarnation.DropGen, opts.IdempotencyKey, hash, string(raw))
				return err
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
	return store.InsertResult{}, fmt.Errorf("%w: table schema changed concurrently; retry the insert", store.ErrInvalid)
}
