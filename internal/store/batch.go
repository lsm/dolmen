package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/secret"
)

type BatchWriteKind string

const (
	BatchWriteInsert      BatchWriteKind = "insert"
	BatchWriteUpdate      BatchWriteKind = "update"
	BatchWriteDelete      BatchWriteKind = "delete"
	BatchWriteUpsert      BatchWriteKind = "upsert"
	BatchWriteUpsertByKey BatchWriteKind = "upsert_by_key"
)

const (
	MaxWritesPerBatch      = 100
	MaxRowsTouchedPerBatch = MaxRecordsPerInsert
)

type BatchWrite struct {
	Kind    BatchWriteKind
	Table   string
	Records []map[string]any
	On      []string
	Filter  string
	Args    []any
	Set     map[string]any
}

type BatchOpts struct {
	IdempotencyKey string
	Owner          string
	RowAccess      string
	Limit          int
	Confirm        bool
	TableWideRead  bool
}

type BatchWriteResult struct {
	Kind     BatchWriteKind
	Ids      []int64
	Inserted int64
	Updated  int64
	Matched  int64
	Deleted  int64
	Changes  ChangeRange
}

type BatchResult struct {
	Replayed bool
	Results  []BatchWriteResult
	Changes  ChangeRange
}

const batchIdemTable = "_dolmen_batches"

const batchIdemDDL = `CREATE TABLE IF NOT EXISTS _dolmen_batches(
	owner TEXT NOT NULL DEFAULT '',
	key TEXT NOT NULL,
	payload_hash TEXT NOT NULL,
	result_json TEXT NOT NULL,
	at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
	PRIMARY KEY(owner, key)
)`

const batchTablesTable = "_dolmen_batch_tables"

const batchTablesDDL = `CREATE TABLE IF NOT EXISTS _dolmen_batch_tables(
	owner TEXT NOT NULL DEFAULT '',
	key TEXT NOT NULL,
	table_name TEXT NOT NULL,
	PRIMARY KEY(owner, key, table_name)
)`

func (r BatchWriteResult) touched() int64 {
	switch r.Kind {
	case BatchWriteInsert, BatchWriteUpsert, BatchWriteUpsertByKey:
		return r.Inserted + r.Updated
	default:
		return r.Updated + r.Matched
	}
}

func fingerprintAny(k *secret.Keyring, v any) any {
	switch t := v.(type) {
	case string:
		return SecretFingerprint(k, t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = fingerprintAny(k, e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for name, e := range t {
			out[name] = fingerprintAny(k, e)
		}
		return out
	default:
		return v
	}
}

func (s *Store) batchPayloadHash(ctx context.Context, q rowQuerier, nsName string, writes []BatchWrite) (string, error) {
	schemas := make(map[string]*schema.TableSchema, len(writes))
	sealed := make([]BatchWrite, 0, len(writes))
	for _, w := range writes {
		sc, ok := schemas[w.Table]
		if !ok {
			var err error
			if sc, err = loadSchema(ctx, q, nsName, w.Table); err != nil {
				return "", err
			}
			schemas[w.Table] = sc
		}
		c := BatchWrite{Kind: w.Kind, Table: w.Table, On: w.On, Filter: w.Filter, Records: w.Records, Args: w.Args, Set: w.Set}
		if len(sc.SecretFields()) > 0 {
			c.Records = FingerprintSecrets(s.secrets, sc, w.Records)
			c.Args, _ = fingerprintAny(s.secrets, w.Args).([]any)
			if m, ok := fingerprintAny(s.secrets, w.Set).(map[string]any); ok {
				c.Set = m
			}
		}
		sealed = append(sealed, c)
	}
	raw, err := json.Marshal(sealed)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func ensureBatchIdem(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, batchIdemDDL); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, batchTablesDDL)
	return err
}

func batchTablesOf(writes []BatchWrite) []string {
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

func purgeBatchRecordsForTable(ctx context.Context, tx *sql.Tx, table string) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM `+batchIdemTable+`
		 WHERE (owner, key) IN (SELECT owner, key FROM `+batchTablesTable+` WHERE table_name = ?)`, table); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM `+batchTablesTable+` WHERE table_name = ?`, table)
	return err
}

func lookupBatchIdem(ctx context.Context, db rowQuerier, owner, key, wantHash string) (BatchResult, bool, error) {
	var gotHash, resultJSON string
	err := db.QueryRowContext(ctx,
		`SELECT payload_hash, result_json FROM `+batchIdemTable+` WHERE owner = ? AND key = ?`,
		owner, key).Scan(&gotHash, &resultJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return BatchResult{}, false, nil
	}
	if err != nil {
		return BatchResult{}, false, err
	}
	if gotHash != wantHash {
		return BatchResult{}, false, conflictf("idempotency key %q was already recorded for a different batch; for a retry, re-send the identical body with the same key (a client-regenerated timestamp or nonce is the classic cause; a fresh key would apply the writes twice); for a genuinely new batch, use a fresh key", key)
	}
	var res BatchResult
	if err := json.Unmarshal([]byte(resultJSON), &res); err != nil {
		return BatchResult{}, false, fmt.Errorf("corrupt batch idempotency record for key %q: %w", key, err)
	}
	res.Replayed = true
	return res, true, nil
}

func storeBatchIdem(ctx context.Context, tx *sql.Tx, owner, key, hash string, tables []string, res BatchResult) error {
	raw, err := json.Marshal(res)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO `+batchIdemTable+` (owner, key, payload_hash, result_json) VALUES (?, ?, ?, ?)`,
		owner, key, hash, string(raw))
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: "+batchIdemTable) {
		return conflictf("idempotency key %q was committed concurrently by another batch; re-send the identical body with the same key", key)
	}
	if err != nil {
		return err
	}
	for _, table := range tables {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO `+batchTablesTable+` (owner, key, table_name) VALUES (?, ?, ?)`,
			owner, key, table); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) guardBatchIncarnation(ctx context.Context, nsName string, want Incarnation) error {
	if want.zero() {
		return nil
	}
	if want.Table != "" {
		return invalidf("a batch spans tables, so it takes a namespace incarnation and not the table incarnation of %q; the version and drop generation of every table it writes are re-checked inside the transaction", want.Table)
	}
	n, err := s.ns(nsName)
	if err != nil {
		return err
	}
	defer n.unpin()
	got, err := readNSGen(ctx, n.ro)
	if err != nil {
		return err
	}
	if got != want.NsGen {
		return conflictf("namespace %q was dropped and recreated between the moment this request's row visibility was decided and its execution; re-read the namespace and retry", nsName)
	}
	return nil
}

func ValidateBatchWrites(writes []BatchWrite) error {
	if len(writes) == 0 {
		return invalidf("no writes given")
	}
	if len(writes) > MaxWritesPerBatch {
		return invalidf("too many writes: %d > %d per batch", len(writes), MaxWritesPerBatch)
	}
	touched := 0
	for i, w := range writes {
		if w.Table == "" {
			return fmt.Errorf("writes[%d]: %w", i, invalidf("table is required"))
		}
		switch w.Kind {
		case BatchWriteInsert, BatchWriteUpsertByKey:
			if len(w.Records) == 0 {
				return fmt.Errorf("writes[%d]: %w", i, invalidf("records is required"))
			}
			if len(w.Records) > MaxRecordsPerInsert {
				return fmt.Errorf("writes[%d]: %w", i, invalidf("too many records: %d > %d per call", len(w.Records), MaxRecordsPerInsert))
			}
			touched += len(w.Records)
		case BatchWriteUpdate, BatchWriteDelete, BatchWriteUpsert:
			if strings.TrimSpace(w.Filter) == "" {
				return fmt.Errorf("writes[%d]: %w", i, invalidf("filter is required (pass \"1=1\" to match everything)"))
			}
		default:
			return fmt.Errorf("writes[%d]: %w", i, invalidf("unknown write kind %q", string(w.Kind)))
		}
		if touched > MaxRowsTouchedPerBatch {
			return fmt.Errorf("writes[%d]: %w", i, invalidf("the batch would touch more than %d rows, which is the budget one insert is allowed", MaxRowsTouchedPerBatch))
		}
	}
	return nil
}

func MergeChangeRanges(into, add ChangeRange) ChangeRange {
	if add.Count == 0 {
		return into
	}
	if into.Count == 0 {
		return add
	}
	if add.First < into.First {
		into.First = add.First
	}
	if add.Last > into.Last {
		into.Last = add.Last
	}
	into.Count += add.Count
	return into
}

func (s *Store) Batch(ctx context.Context, nsName string, writes []BatchWrite, opts BatchOpts, emb Embedder, scope *RowScope, scopeIncarnation Incarnation) (_ BatchResult, err error) {
	ctx, span := s.tr.Op(ctx, "BATCH", nsName, "")
	defer func() { s.tr.End(ctx, span, err) }()

	if err := s.guardBatchIncarnation(ctx, nsName, scopeIncarnation); err != nil {
		return BatchResult{}, err
	}
	if len(opts.IdempotencyKey) > MaxIdempotencyKeyLen {
		return BatchResult{}, invalidf("idempotency key is %d bytes (max %d)", len(opts.IdempotencyKey), MaxIdempotencyKeyLen)
	}
	if err := ValidateBatchWrites(writes); err != nil {
		return BatchResult{}, err
	}

	n, err := s.ns(nsName)
	if err != nil {
		return BatchResult{}, err
	}
	defer n.unpin()

	for attempt := 0; ; attempt++ {
		res, done, err := s.batchAttempt(ctx, n, nsName, writes, opts, emb, scope)
		if done {
			if err != nil {
				return BatchResult{}, err
			}
			if !res.Replayed {
				s.notifyCommitted(nsName, "", res.Changes)
			}
			return res, nil
		}
		if attempt >= 2 {
			return BatchResult{}, invalidf("a table changed while the batch was committing; the whole batch was rolled back, so re-send it")
		}
	}
}

func (s *Store) batchAttempt(ctx context.Context, n *nsDB, nsName string, writes []BatchWrite, opts BatchOpts, emb Embedder, scope *RowScope) (BatchResult, bool, error) {
	ctx, wt, err := s.writeTxFor(ctx, n, nil)
	if err != nil {
		return BatchResult{}, true, err
	}
	defer s.releaseWrite(wt)
	inner := &sharedWriteTx{tx: wt.tx, span: wt.span}

	var hash string
	if opts.IdempotencyKey != "" {
		hash, err = s.batchPayloadHash(ctx, wt.tx, nsName, writes)
		if err != nil {
			return BatchResult{}, true, err
		}
		res, found, err := lookupBatchIdem(ctx, wt.tx, opts.Owner, opts.IdempotencyKey, hash)
		if err != nil {
			return BatchResult{}, true, err
		}
		if found {
			return res, true, nil
		}
	}

	wopts := WriteOpts{Owner: opts.Owner, TableWideRead: opts.TableWideRead}
	dopts := DeleteOpts{Limit: opts.Limit, Confirm: opts.Confirm}
	results := make([]BatchWriteResult, 0, len(writes))
	var changes ChangeRange
	touched := int64(0)

	for i, w := range writes {
		out := BatchWriteResult{Kind: w.Kind}
		switch w.Kind {
		case BatchWriteInsert:
			ids, ch, _, done, err := s.insert(ctx, nsName, w.Table, w.Records, emb, "", opts.Owner, DomainFor(wopts, scope), inner)
			if !done {
				return BatchResult{}, false, nil
			}
			if err != nil {
				return BatchResult{}, true, fmt.Errorf("writes[%d]: %w", i, err)
			}
			out.Ids, out.Inserted, out.Changes = ids, int64(len(ids)), ch
		case BatchWriteUpsertByKey:
			r, done, err := s.upsertByKey(ctx, nsName, w.Table, w.On, w.Records, wopts, emb, scope, Incarnation{}, inner)
			if !done {
				return BatchResult{}, false, nil
			}
			if err != nil {
				return BatchResult{}, true, fmt.Errorf("writes[%d]: %w", i, err)
			}
			out.Ids, out.Inserted, out.Updated, out.Changes = r.Ids, r.Inserted, r.Updated, r.Changes
		case BatchWriteUpdate:
			r, done, err := s.updateOrUpsert(ctx, nsName, w.Table, w.Filter, w.Args, w.Set, emb, false, "", scope, Incarnation{}, inner)
			if !done {
				return BatchResult{}, false, nil
			}
			if err != nil {
				return BatchResult{}, true, fmt.Errorf("writes[%d]: %w", i, err)
			}
			out.Updated, out.Changes = r.Updated, r.Changes
		case BatchWriteUpsert:
			r, done, err := s.updateOrUpsert(ctx, nsName, w.Table, w.Filter, w.Args, w.Set, emb, true, opts.Owner, scope, Incarnation{}, inner)
			if !done {
				return BatchResult{}, false, nil
			}
			if err != nil {
				return BatchResult{}, true, fmt.Errorf("writes[%d]: %w", i, err)
			}
			out.Ids, out.Inserted, out.Updated, out.Changes = r.Ids, r.Inserted, r.Updated, r.Changes
		case BatchWriteDelete:
			where, err := normalizeDeleteFilter(w.Filter, w.Args)
			if err != nil {
				return BatchResult{}, true, fmt.Errorf("writes[%d]: %w", i, err)
			}
			r, _, err := s.deleteWith(ctx, n, nsName, w.Table, where, w.Args, dopts, scope, Incarnation{}, inner)
			if err != nil {
				return BatchResult{}, true, fmt.Errorf("writes[%d]: %w", i, err)
			}
			out.Matched, out.Deleted, out.Changes = r.Matched, r.Deleted, r.Changes
		}
		touched += out.touched()
		if touched > MaxRowsTouchedPerBatch {
			return BatchResult{}, true, fmt.Errorf("writes[%d]: %w", i,
				invalidf("the batch would touch more than %d rows, which is the budget one insert is allowed", MaxRowsTouchedPerBatch))
		}
		results = append(results, out)
		changes = MergeChangeRanges(changes, out.Changes)
	}

	res := BatchResult{Results: results, Changes: changes}
	if opts.IdempotencyKey != "" {
		if err := storeBatchIdem(ctx, wt.tx, opts.Owner, opts.IdempotencyKey, hash, batchTablesOf(writes), res); err != nil {
			return BatchResult{}, true, err
		}
	}
	if err := s.commitOwned(wt); err != nil {
		return BatchResult{}, true, err
	}
	return res, true, nil
}
