package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/store"
)

var errBatchRetry = errors.New("a table changed while the batch was committing")

const batchAttempts = 3

func batchPayloadHash(writes []store.BatchWrite) string {
	raw, err := json.Marshal(writes)
	if err != nil {
		raw = []byte("marshal error: " + err.Error())
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (s *Store) lookupBatchIdem(ctx context.Context, tx pgx.Tx, ns, owner, key, wantHash string) (store.BatchResult, bool, error) {
	var gotHash, resultJSON string
	err := tx.QueryRow(ctx,
		"SELECT payload_hash, result_json FROM "+s.relation("batches")+" WHERE namespace=$1 AND owner=$2 AND key=$3",
		ns, owner, key).Scan(&gotHash, &resultJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.BatchResult{}, false, nil
	}
	if err != nil {
		return store.BatchResult{}, false, err
	}
	if gotHash != wantHash {
		return store.BatchResult{}, false, derr.New(derr.Conflict, "idempotency key %q was already recorded for a different batch; for a retry, re-send the identical body with the same key (a client-regenerated timestamp or nonce is the classic cause; a fresh key would apply the writes twice); for a genuinely new batch, use a fresh key", key)
	}
	var res store.BatchResult
	if err := json.Unmarshal([]byte(resultJSON), &res); err != nil {
		return store.BatchResult{}, false, fmt.Errorf("corrupt batch idempotency record for key %q: %w", key, err)
	}
	res.Replayed = true
	return res, true, nil
}

func (s *Store) storeBatchIdem(ctx context.Context, tx pgx.Tx, ns, owner, key, hash string, tables []string, res store.BatchResult) error {
	raw, err := json.Marshal(res)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx,
		"INSERT INTO "+s.relation("batches")+" (namespace,owner,key,payload_hash,result_json) VALUES($1,$2,$3,$4,$5)",
		ns, owner, key, hash, string(raw))
	if err != nil {
		return err
	}
	for _, table := range tables {
		if _, err := tx.Exec(ctx,
			"INSERT INTO "+s.relation("batch_tables")+" (namespace,owner,key,table_name) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING",
			ns, owner, key, table); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) purgeBatchRecordsForTable(ctx context.Context, tx pgx.Tx, ns, table string) error {
	if _, err := tx.Exec(ctx,
		"DELETE FROM "+s.relation("batches")+
			" WHERE namespace=$1 AND (owner, key) IN (SELECT owner, key FROM "+s.relation("batch_tables")+" WHERE namespace=$1 AND table_name=$2)",
		ns, table); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "DELETE FROM "+s.relation("batch_tables")+" WHERE namespace=$1 AND table_name=$2", ns, table)
	return err
}

func batchTablesOf(writes []store.BatchWrite) []string {
	seen := make(map[string]bool, len(writes))
	out := make([]string, 0, len(writes))
	for _, w := range writes {
		if seen[w.Table] {
			continue
		}
		seen[w.Table] = true
		out = append(out, w.Table)
	}
	return out
}

func batchTouched(r store.BatchWriteResult) int64 {
	switch r.Kind {
	case store.BatchWriteInsert, store.BatchWriteUpsert, store.BatchWriteUpsertByKey:
		return r.Inserted + r.Updated
	default:
		return r.Updated + r.Matched
	}
}

func (s *Store) guardBatchIncarnation(ns string, want store.Incarnation) error {
	if store.IncarnationIsZero(want) {
		return nil
	}
	if want.Table != "" {
		return fmt.Errorf("%w: a batch spans tables, so it takes a namespace incarnation and not the table incarnation of %q; the version and drop generation of every table it writes are re-checked inside the transaction", store.ErrInvalid, want.Table)
	}
	return nil
}

func (s *Store) Batch(ctx context.Context, ns string, writes []store.BatchWrite, opts store.BatchOpts, emb store.Embedder, scope *store.RowScope, expected store.Incarnation) (_ store.BatchResult, err error) {
	ctx, end := s.span(ctx, "BATCH", ns, "")
	defer func() { end(err) }()

	if err := s.guardBatchIncarnation(ns, expected); err != nil {
		return store.BatchResult{}, err
	}
	if len(opts.IdempotencyKey) > store.MaxIdempotencyKeyLen {
		return store.BatchResult{}, fmt.Errorf("%w: idempotency key is %d bytes (max %d)", store.ErrInvalid, len(opts.IdempotencyKey), store.MaxIdempotencyKeyLen)
	}
	if err := store.ValidateBatchWrites(writes); err != nil {
		return store.BatchResult{}, err
	}

	hash := batchPayloadHash(writes)
	var replayed store.BatchResult
	if opts.IdempotencyKey != "" {
		err := s.write(ctx, ns, [16]byte{}, func(tx pgx.Tx, n namespace) error {
			res, found, err := s.lookupBatchIdem(ctx, tx, ns, opts.Owner, opts.IdempotencyKey, hash)
			if err != nil || !found {
				return err
			}
			replayed = res
			return nil
		})
		if err != nil {
			return store.BatchResult{}, err
		}
		if replayed.Results != nil || replayed.Replayed {
			return replayed, nil
		}
	}

	for attempt := 0; ; attempt++ {
		res, retry, err := s.batchAttempt(ctx, ns, writes, opts, emb, scope, hash)
		if !retry {
			if err != nil {
				return store.BatchResult{}, err
			}
			return res, nil
		}
		if attempt >= batchAttempts-1 {
			return store.BatchResult{}, fmt.Errorf("%w: a table changed while the batch was committing; the whole batch was rolled back, so re-send it", store.ErrInvalid)
		}
	}
}

func (s *Store) batchAttempt(ctx context.Context, ns string, writes []store.BatchWrite, opts store.BatchOpts, emb store.Embedder, scope *store.RowScope, hash string) (store.BatchResult, bool, error) {
	var (
		res   store.BatchResult
		retry bool
	)
	err := s.write(ctx, ns, [16]byte{}, func(tx pgx.Tx, n namespace) error {
		inner := withCarriedTx(ctx, tx, n)
		wopts := store.WriteOpts{Owner: opts.Owner, TableWideRead: opts.TableWideRead}
		dopts := store.DeleteOpts{Limit: opts.Limit, Confirm: opts.Confirm}
		results := make([]store.BatchWriteResult, 0, len(writes))
		var changes store.ChangeRange
		var touched int64

		if opts.IdempotencyKey != "" {
			prev, found, err := s.lookupBatchIdem(ctx, tx, ns, opts.Owner, opts.IdempotencyKey, hash)
			if err != nil {
				return err
			}
			if found {
				res, retry = prev, false
				return nil
			}
		}

		for i, w := range writes {
			out := store.BatchWriteResult{Kind: w.Kind}
			switch w.Kind {
			case store.BatchWriteInsert:
				r, err := s.Insert(inner, ns, w.Table, w.Records, wopts, emb, scope, store.Incarnation{})
				if errors.Is(err, errBatchRetry) {
					retry = true
					return nil
				}
				if err != nil {
					return fmt.Errorf("writes[%d]: %w", i, err)
				}
				out.Ids, out.Inserted, out.Changes = r.Ids, int64(len(r.Ids)), r.Changes
			case store.BatchWriteUpsertByKey:
				r, err := s.UpsertByKey(inner, ns, w.Table, w.On, w.Records, wopts, emb, scope, store.Incarnation{})
				if errors.Is(err, errBatchRetry) {
					retry = true
					return nil
				}
				if err != nil {
					return fmt.Errorf("writes[%d]: %w", i, err)
				}
				out.Ids, out.Inserted, out.Updated, out.Changes = r.Ids, r.Inserted, r.Updated, r.Changes
			case store.BatchWriteUpdate:
				r, err := s.Update(inner, ns, w.Table, w.Filter, w.Args, w.Set, emb, scope, store.Incarnation{})
				if errors.Is(err, errBatchRetry) {
					retry = true
					return nil
				}
				if err != nil {
					return fmt.Errorf("writes[%d]: %w", i, err)
				}
				out.Updated, out.Changes = r.Updated, r.Changes
			case store.BatchWriteUpsert:
				r, err := s.Upsert(inner, ns, w.Table, w.Filter, w.Args, w.Set, wopts, emb, scope, store.Incarnation{})
				if errors.Is(err, errBatchRetry) {
					retry = true
					return nil
				}
				if err != nil {
					return fmt.Errorf("writes[%d]: %w", i, err)
				}
				out.Ids, out.Inserted, out.Updated, out.Changes = r.Ids, r.Inserted, r.Updated, r.Changes
			case store.BatchWriteDelete:
				r, err := s.Delete(inner, ns, w.Table, w.Filter, w.Args, dopts, scope, store.Incarnation{})
				if err != nil {
					return fmt.Errorf("writes[%d]: %w", i, err)
				}
				out.Matched, out.Deleted, out.Changes = r.Matched, r.Deleted, r.Changes
			}
			touched += batchTouched(out)
			if touched > store.MaxRowsTouchedPerBatch {
				return fmt.Errorf("writes[%d]: %w", i,
					fmt.Errorf("%w: the batch would touch more than %d rows, which is the budget one insert is allowed", store.ErrInvalid, store.MaxRowsTouchedPerBatch))
			}
			results = append(results, out)
			changes = store.MergeChangeRanges(changes, out.Changes)
		}

		res = store.BatchResult{Results: results, Changes: changes}
		if opts.IdempotencyKey != "" {
			if err := s.storeBatchIdem(ctx, tx, ns, opts.Owner, opts.IdempotencyKey, hash, batchTablesOf(writes), res); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return store.BatchResult{}, false, err
	}
	if retry {
		return store.BatchResult{}, true, nil
	}
	return res, false, nil
}
