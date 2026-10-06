package lakehouse

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/apache/iceberg-go/table"
	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type batchKey struct{}

const batchJournal = "catalog.db.batch"

func (s *Store) journalPath(ns string) string {
	return filepath.Join(s.dir, namespacePath(ns), batchJournal)
}

func (s *Store) Batch(ctx context.Context, ns string, writes []store.BatchWrite, opts store.BatchOpts, emb store.Embedder, scope *store.RowScope, scopeIncarnation store.Incarnation) (store.BatchResult, error) {
	if scopeIncarnation.Table != "" {
		return store.BatchResult{}, invalidf("a batch spans tables, so it takes a namespace incarnation and not the table incarnation of %q; the version and drop generation of every table it writes are re-checked inside the transaction", scopeIncarnation.Table)
	}
	if len(opts.IdempotencyKey) > store.MaxIdempotencyKeyLen {
		return store.BatchResult{}, invalidf("idempotency key is %d bytes (max %d)", len(opts.IdempotencyKey), store.MaxIdempotencyKeyLen)
	}
	if err := store.ValidateBatchWrites(writes); err != nil {
		return store.BatchResult{}, err
	}
	var result store.BatchResult
	err := s.withNamespaceExclusive(ctx, ns, func(n *namespace) error {
		if scopeIncarnation.NsGen != [16]byte{} && scopeIncarnation.NsGen != n.generation {
			return derr.New(derr.Conflict, "namespace %q was dropped and recreated between the moment this request's row visibility was decided and its execution; re-read the namespace and retry", ns)
		}
		var hash store.IdemHash
		if opts.IdempotencyKey != "" {
			schemas := map[string]*schema.TableSchema{}
			for i, w := range writes {
				if _, ok := schemas[w.Table]; ok {
					continue
				}
				state, err := loadTable(ctx, n, ns, w.Table)
				if err != nil {
					return fmt.Errorf("writes[%d]: %w", i, err)
				}
				schemas[w.Table] = state.schema
			}
			var err error
			if hash, err = store.BatchPayloadHash(s.secrets, schemas, writes, opts); err != nil {
				return err
			}
			replay, found, err := lookupBatch(ctx, n.db, opts.Owner, opts.IdempotencyKey, hash)
			if err != nil || found {
				result = replay
				return err
			}
		}
		if err := s.beginBatch(ctx, n, ns); err != nil {
			return err
		}
		res, err := s.runBatch(context.WithValue(ctx, batchKey{}, ns), ns, writes, opts, emb, scope)
		if err == nil {
			n = s.namespaces[ns]
			err = s.commitBatch(ctx, n, opts, hash, res, batchTables(writes))
		}
		if err != nil {
			if rerr := s.rollbackBatch(context.WithoutCancel(ctx), ns); rerr != nil {
				return errors.Join(err, rerr)
			}
			return err
		}
		os.Remove(s.journalPath(ns))
		result = res
		return nil
	})
	return result, err
}

func lookupBatch(ctx context.Context, db *sql.DB, owner, key string, hash store.IdemHash) (store.BatchResult, bool, error) {
	var stored, raw string
	err := db.QueryRowContext(ctx, `SELECT payload_hash, result FROM _dolmen_lakehouse_batches WHERE owner = ? AND key = ?`, owner, key).Scan(&stored, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return store.BatchResult{}, false, nil
	}
	if err != nil {
		return store.BatchResult{}, false, err
	}
	if !hash.Matches(stored) {
		return store.BatchResult{}, false, derr.New(derr.Conflict, "idempotency key %q was already recorded for a different batch; for a retry, re-send the identical body with the same key (a client-regenerated timestamp or nonce is the classic cause; a fresh key would apply the writes twice); for a genuinely new batch, use a fresh key", key)
	}
	var res store.BatchResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		return store.BatchResult{}, false, fmt.Errorf("%w: corrupt lakehouse batch idempotency record for key %q: %v", store.ErrCatalogCorrupt, key, err)
	}
	res.Replayed = true
	return res, true, nil
}

func (s *Store) beginBatch(ctx context.Context, n *namespace, ns string) error {
	path := s.journalPath(ns)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if _, err := n.db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("snapshot the lakehouse catalog before a batch: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	if err := s.syncDirectory(namespacePath(ns)); err != nil {
		return err
	}
	_, err = n.db.ExecContext(ctx, `INSERT INTO _dolmen_lakehouse_batch_intent(singleton) VALUES(1)`)
	return err
}

func (s *Store) runBatch(ctx context.Context, ns string, writes []store.BatchWrite, opts store.BatchOpts, emb store.Embedder, scope *store.RowScope) (store.BatchResult, error) {
	wopts := store.WriteOpts{Owner: opts.Owner, TableWideRead: opts.TableWideRead}
	dopts := store.DeleteOpts{Limit: opts.Limit, Confirm: opts.Confirm, NoDryRunAdvice: true}
	results := make([]store.BatchWriteResult, 0, len(writes))
	var changes store.ChangeRange
	touched := int64(0)
	for i, w := range writes {
		out := store.BatchWriteResult{Kind: w.Kind}
		ws := store.WriteScope(w, scope)
		var err error
		switch w.Kind {
		case store.BatchWriteInsert:
			var r store.InsertResult
			r, err = s.Insert(ctx, ns, w.Table, w.Records, wopts, emb, ws, w.Incarnation)
			out.Ids, out.Inserted, out.Changes = r.Ids, int64(len(r.Ids)), r.Changes
		case store.BatchWriteUpsertByKey:
			var r store.InsertResult
			r, err = s.UpsertByKey(ctx, ns, w.Table, w.On, w.Records, wopts, emb, ws, w.Incarnation)
			out.Ids, out.Inserted, out.Updated, out.Changes = r.Ids, r.Inserted, r.Updated, r.Changes
		case store.BatchWriteUpdate:
			var r store.UpdateResult
			r, err = s.Update(ctx, ns, w.Table, w.Filter, w.Args, w.Set, emb, ws, w.Incarnation)
			out.Updated, out.Changes = r.Updated, r.Changes
		case store.BatchWriteUpsert:
			var r store.InsertResult
			r, err = s.Upsert(ctx, ns, w.Table, w.Filter, w.Args, w.Set, wopts, emb, ws, w.Incarnation)
			out.Ids, out.Inserted, out.Updated, out.Changes = r.Ids, r.Inserted, r.Updated, r.Changes
		case store.BatchWriteDelete:
			var r store.DeleteResult
			r, err = s.Delete(ctx, ns, w.Table, w.Filter, w.Args, dopts, ws, w.Incarnation)
			out.Matched, out.Deleted, out.Changes = r.Matched, r.Deleted, r.Changes
		}
		if err != nil {
			return store.BatchResult{}, fmt.Errorf("writes[%d]: %w", i, err)
		}
		touched += store.BatchTouched(out)
		if touched > store.MaxRowsTouchedPerBatch {
			return store.BatchResult{}, fmt.Errorf("writes[%d]: %w", i, invalidf(store.BatchBudgetMessage, store.MaxRowsTouchedPerBatch))
		}
		results = append(results, out)
		changes = store.MergeChangeRanges(changes, out.Changes)
	}
	return store.BatchResult{Results: results, Changes: changes}, nil
}

func (s *Store) commitBatch(ctx context.Context, n *namespace, opts store.BatchOpts, hash store.IdemHash, res store.BatchResult, tables []string) error {
	tx, err := n.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if res.Changes.Count > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE _dolmen_lakehouse_changes SET commit_id = (SELECT commit_id FROM _dolmen_lakehouse_changes WHERE seq = ?) WHERE seq BETWEEN ? AND ?`, res.Changes.First, res.Changes.First, res.Changes.Last); err != nil {
			return err
		}
	}
	if opts.IdempotencyKey != "" {
		raw, err := json.Marshal(res)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO _dolmen_lakehouse_batches(owner, key, payload_hash, result) VALUES(?,?,?,?)`, opts.Owner, opts.IdempotencyKey, hash.Primary, string(raw)); err != nil {
			return err
		}
		for _, table := range tables {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO _dolmen_lakehouse_batch_tables(owner, key, table_name) VALUES(?,?,?)`, opts.Owner, opts.IdempotencyKey, table); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM _dolmen_lakehouse_batch_intent`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) rollbackBatch(ctx context.Context, ns string) error {
	if err := s.evict(ns); err != nil {
		s.unsetNamespace(ns)
		return err
	}
	if err := s.restoreJournal(ns); err != nil {
		return err
	}
	n, err := s.openCatalog(ctx, ns, namespacePath(ns), false)
	if err != nil {
		return err
	}
	if err := s.republish(ctx, n, ns); err != nil {
		n.db.Close()
		return err
	}
	s.tick++
	n.lastUse = s.tick
	s.setNamespace(ns, n)
	return nil
}

func (s *Store) restoreJournal(ns string) error {
	dir := filepath.Join(s.dir, namespacePath(ns))
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(filepath.Join(dir, "catalog.db"+suffix)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Rename(filepath.Join(dir, batchJournal), filepath.Join(dir, "catalog.db")); err != nil {
		return fmt.Errorf("restore the lakehouse catalog after a failed batch: %w", err)
	}
	if err := s.syncDirectory(namespacePath(ns)); err != nil {
		return err
	}
	return nil
}

func (s *Store) republish(ctx context.Context, n *namespace, ns string) error {
	for ident, err := range n.catalog.ListTables(ctx, table.Identifier(strings.Split(ns, "/"))) {
		if err != nil {
			return err
		}
		state, err := loadTable(ctx, n, ns, ident[len(ident)-1])
		if err != nil {
			return err
		}
		if err := s.syncMetadata(state.native, n); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) recoverBatch(ctx context.Context, ns string, n *namespace) (*namespace, error) {
	var pending int
	if err := n.db.QueryRowContext(ctx, `SELECT count(*) FROM _dolmen_lakehouse_batch_intent`).Scan(&pending); err != nil {
		n.db.Close()
		return nil, err
	}
	if pending == 0 {
		if err := os.Remove(s.journalPath(ns)); err != nil && !os.IsNotExist(err) {
			n.db.Close()
			return nil, err
		}
		return n, nil
	}
	if err := n.db.Close(); err != nil {
		return nil, err
	}
	if err := s.restoreJournal(ns); err != nil {
		return nil, err
	}
	n, err := s.openCatalog(ctx, ns, namespacePath(ns), false)
	if err != nil {
		return nil, err
	}
	if err := s.republish(ctx, n, ns); err != nil {
		n.db.Close()
		return nil, err
	}
	return n, nil
}

func batchTables(writes []store.BatchWrite) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range writes {
		if !seen[w.Table] {
			seen[w.Table] = true
			out = append(out, w.Table)
		}
	}
	return out
}
