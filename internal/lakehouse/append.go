package lakehouse

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"github.com/apache/iceberg-go"
	"github.com/apache/iceberg-go/table"
	"github.com/google/uuid"
	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
	"github.com/lsm/dolmen/internal/value"
)

const commitProperty = "dolmen.commit"

var appendDDL = []string{
	`CREATE TABLE IF NOT EXISTS _dolmen_lakehouse_ids(table_name TEXT NOT NULL, generation INTEGER NOT NULL, next_id INTEGER NOT NULL CHECK(next_id >= 1), PRIMARY KEY(table_name, generation))`,
	`CREATE TABLE IF NOT EXISTS _dolmen_lakehouse_commits(commit_id INTEGER PRIMARY KEY AUTOINCREMENT, table_name TEXT NOT NULL, generation INTEGER NOT NULL, kind TEXT NOT NULL, rows BLOB NOT NULL, materialized INTEGER NOT NULL DEFAULT 0)`,
	`CREATE INDEX IF NOT EXISTS _dolmen_lakehouse_commits_pending ON _dolmen_lakehouse_commits(commit_id) WHERE materialized = 0`,
	`CREATE TABLE IF NOT EXISTS _dolmen_lakehouse_changes(seq INTEGER PRIMARY KEY AUTOINCREMENT, table_name TEXT NOT NULL, generation INTEGER NOT NULL, row_id INTEGER NOT NULL, kind TEXT NOT NULL, owner TEXT, commit_id INTEGER NOT NULL, at TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS _dolmen_lakehouse_idempotency(table_name TEXT NOT NULL, generation INTEGER NOT NULL, owner TEXT NOT NULL, key TEXT NOT NULL, payload_hash TEXT NOT NULL, result TEXT NOT NULL, PRIMARY KEY(table_name, generation, owner, key))`,
	`CREATE TABLE IF NOT EXISTS _dolmen_lakehouse_secrets(table_name TEXT NOT NULL, generation INTEGER NOT NULL, row_id INTEGER NOT NULL, field TEXT NOT NULL, value BLOB NOT NULL, PRIMARY KEY(table_name, generation, row_id, field))`,
	`CREATE TABLE IF NOT EXISTS _dolmen_lakehouse_cursors(token TEXT PRIMARY KEY, position INTEGER NOT NULL, chain_origin INTEGER NOT NULL, chain_start INTEGER NOT NULL, issued_at INTEGER NOT NULL, table_name TEXT NOT NULL, drop_generation INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS _dolmen_lakehouse_batches(owner TEXT NOT NULL, key TEXT NOT NULL, payload_hash TEXT NOT NULL, result TEXT NOT NULL, PRIMARY KEY(owner, key))`,
	`CREATE TABLE IF NOT EXISTS _dolmen_lakehouse_batch_tables(owner TEXT NOT NULL, key TEXT NOT NULL, table_name TEXT NOT NULL, PRIMARY KEY(owner, key, table_name))`,
	`CREATE TABLE IF NOT EXISTS _dolmen_lakehouse_batch_intent(singleton INTEGER PRIMARY KEY CHECK(singleton = 1))`,
	`CREATE TABLE IF NOT EXISTS _dolmen_lakehouse_embed_stage(table_name TEXT NOT NULL, generation INTEGER NOT NULL, provider TEXT NOT NULL, row_id INTEGER NOT NULL, digest BLOB NOT NULL, vector BLOB NOT NULL, PRIMARY KEY(table_name, generation, provider, row_id))`,
	`CREATE TABLE IF NOT EXISTS _dolmen_lakehouse_counts(table_name TEXT NOT NULL, generation INTEGER NOT NULL, owner TEXT NOT NULL, n INTEGER NOT NULL CHECK(n >= 0), PRIMARY KEY(table_name, generation, owner))`,
}

var materializeHook func() error

var secretPresent = []byte{0}

type commitRows struct {
	Rows   []map[string]any
	Delete []int64
}

func idFilter(ids []int64) iceberg.BooleanExpression {
	return iceberg.IsIn(iceberg.Reference("id"), ids...)
}

func ensureAppendTables(ctx context.Context, db *sql.DB) error {
	for _, stmt := range appendDDL {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func pendingCommits(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM _dolmen_lakehouse_commits WHERE materialized = 0`).Scan(&n)
	return n, err
}

func normalizeRecords(records []map[string]any) ([]map[string]any, error) {
	if len(records) == 0 {
		return nil, invalidf("no records given")
	}
	if len(records) > store.MaxRecordsPerInsert {
		return nil, invalidf("too many records: %d > %d per call", len(records), store.MaxRecordsPerInsert)
	}
	out := make([]map[string]any, len(records))
	for i, rec := range records {
		if rec == nil {
			return nil, invalidf("records[%d] must be an object, not null", i)
		}
		out[i] = map[string]any{}
		for k, v := range rec {
			key := strings.ToLower(k)
			if _, exists := out[i][key]; exists {
				return nil, invalidf("record %d: fields %q and its case variant collapse to %q; use one spelling", i, k, key)
			}
			out[i][key] = v
		}
	}
	return out, nil
}

func canonicalNumber(v any) string {
	f, ok := v.(float64)
	if !ok {
		return fmt.Sprint(v)
	}
	switch {
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	text := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(text, ".eEN") {
		text += ".0"
	}
	return text
}

func (s *Store) prepareRows(ctx context.Context, state tableState, records []map[string]any, emb store.Embedder, stamp, owner string) ([]map[string]any, error) {
	sc := state.schema
	out := make([]map[string]any, len(records))
	texts := []string{}
	indices := []int{}
	vf := sc.VectorizeField()
	for i, rec := range records {
		for name := range rec {
			if sc.Field(name) == nil {
				return nil, invalidf("unknown field %q on table %s (see describe_table)", name, sc.Name)
			}
		}
		row := map[string]any{"created_at": stamp}
		for _, f := range sc.Fields {
			v, present := rec[f.Name]
			if !present && f.Default != nil {
				v = f.Default
				if f.Type == schema.Timestamp && schema.IsNowDefault(v) {
					v = stamp
				}
			}
			if v == nil {
				if f.Required {
					return nil, invalidf("field %q is required", f.Name)
				}
				continue
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
				sealed, err := store.SealSecret(s.secrets, f, coerced)
				if err != nil {
					return nil, err
				}
				coerced = sealed
			}
			if coerced == nil {
				continue
			}
			row[f.Name] = coerced
			if vf != nil && f.Name == vf.Name {
				if text, ok := coerced.(string); ok && text != "" {
					texts = append(texts, text)
					indices = append(indices, i)
				}
			}
		}
		if sc.HasOwner {
			row[schema.OwnerColumn] = owner
		}
		out[i] = row
	}
	if len(texts) > 0 {
		vecs, err := store.EmbedTexts(ctx, sc, sc.Name, texts, emb)
		if err != nil {
			return nil, err
		}
		for j, i := range indices {
			out[i]["_embedding"] = schema.EncodeVector(vecs[j])
		}
	}
	return out, nil
}

func lookupIdempotency(ctx context.Context, tx interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, state tableState, key string, hash store.IdemHash, owner string) (store.InsertResult, bool, error) {
	var result store.InsertResult
	var stored, raw string
	err := tx.QueryRowContext(ctx, `SELECT payload_hash, result FROM _dolmen_lakehouse_idempotency WHERE table_name=? AND generation=? AND owner=? AND key=?`, state.incarnation.Table, state.incarnation.DropGen, owner, key).Scan(&stored, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return result, false, nil
	}
	if err != nil {
		return result, false, err
	}
	if !hash.Matches(stored) {
		return result, false, derr.New(derr.Conflict, "idempotency key %q was already recorded for a different insert into %s; re-send the identical body for a retry, or use a fresh key for a new insert", key, state.incarnation.Table)
	}
	if err := json.Unmarshal([]byte(raw), &result.Ids); err != nil {
		return result, false, fmt.Errorf("%w: corrupt lakehouse idempotency record: %v", store.ErrCatalogCorrupt, err)
	}
	result.Replayed = true
	return result, true, nil
}

func (s *Store) Insert(ctx context.Context, ns, name string, records []map[string]any, opts store.WriteOpts, emb store.Embedder, scope *store.RowScope, scopeIncarnation store.Incarnation) (store.InsertResult, error) {
	if len(opts.IdempotencyKey) > store.MaxIdempotencyKeyLen {
		return store.InsertResult{}, invalidf("idempotency key is %d bytes (max %d)", len(opts.IdempotencyKey), store.MaxIdempotencyKeyLen)
	}
	records, err := normalizeRecords(records)
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
		domain := store.DomainFor(opts, scope)
		var hash store.IdemHash
		if opts.IdempotencyKey != "" {
			if hash, err = store.RequestHash(s.secrets, state.schema, records); err != nil {
				return err
			}
		}
		replayed := func(q interface {
			QueryRowContext(context.Context, string, ...any) *sql.Row
		}) (bool, error) {
			if opts.IdempotencyKey == "" {
				return false, nil
			}
			replay, found, err := lookupIdempotency(ctx, q, state, opts.IdempotencyKey, hash, domain.Owner)
			if err == nil && !found && domain.FallsBackToLegacy() {
				replay, found, err = lookupIdempotency(ctx, q, state, opts.IdempotencyKey, hash, store.LegacyIdempotencyOwner)
			}
			if found {
				result = replay
			}
			return found, err
		}
		if found, err := replayed(n.db); err != nil || found {
			return err
		}
		stamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
		space, dim := state.schema.EmbedSpace, state.schema.EmbedDim
		rows, err := s.prepareRows(ctx, state, records, emb, stamp, opts.Owner)
		if err != nil {
			return err
		}
		if state.schema.EmbedDim != dim || space == "" && state.schema.EmbedDim != 0 {
			state.schema.EmbedSpace = emb.Identity
			if err := s.publishSchema(ctx, n, state); err != nil {
				return err
			}
		}
		tx, err := n.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if found, err := replayed(tx); err != nil || found {
			return err
		}
		committed, err := s.commitAppend(ctx, tx, state, rows, opts, domain, hash, stamp)
		if err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		n.pending++
		result = committed
		s.materialize(ctx, n, ns)
		return nil
	})
	return result, err
}

func (s *Store) commitAppend(ctx context.Context, tx *sql.Tx, state tableState, rows []map[string]any, opts store.WriteOpts, domain store.IdemDomain, hash store.IdemHash, stamp string) (store.InsertResult, error) {
	var result store.InsertResult
	inc := state.incarnation
	var next int64
	err := tx.QueryRowContext(ctx, `SELECT next_id FROM _dolmen_lakehouse_ids WHERE table_name=? AND generation=?`, inc.Table, inc.DropGen).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		next = 1
	} else if err != nil {
		return result, err
	}
	result.Ids = make([]int64, len(rows))
	for i, row := range rows {
		result.Ids[i] = next + int64(i)
		row["id"] = result.Ids[i]
		for _, f := range state.schema.SecretFields() {
			sealed, ok := row[f.Name].([]byte)
			if !ok {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO _dolmen_lakehouse_secrets(table_name, generation, row_id, field, value) VALUES(?,?,?,?,?)`, inc.Table, inc.DropGen, result.Ids[i], f.Name, sealed); err != nil {
				return result, err
			}
			row[f.Name] = secretPresent
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO _dolmen_lakehouse_ids(table_name, generation, next_id) VALUES(?,?,?) ON CONFLICT(table_name, generation) DO UPDATE SET next_id=excluded.next_id`, inc.Table, inc.DropGen, next+int64(len(rows))); err != nil {
		return result, err
	}
	var payload bytes.Buffer
	if err := gob.NewEncoder(&payload).Encode(commitRows{Rows: rows}); err != nil {
		return result, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO _dolmen_lakehouse_commits(table_name, generation, kind, rows) VALUES(?,?,?,?)`, inc.Table, inc.DropGen, string(store.ChangeInsert), payload.Bytes())
	if err != nil {
		return result, err
	}
	commit, err := res.LastInsertId()
	if err != nil {
		return result, err
	}
	var owner any
	if state.schema.HasOwner {
		owner = opts.Owner
	}
	for i, id := range result.Ids {
		res, err := tx.ExecContext(ctx, `INSERT INTO _dolmen_lakehouse_changes(table_name, generation, row_id, kind, owner, commit_id, at) VALUES(?,?,?,?,?,?,?)`, inc.Table, inc.DropGen, id, string(store.ChangeInsert), owner, commit, stamp)
		if err != nil {
			return result, err
		}
		seq, err := res.LastInsertId()
		if err != nil {
			return result, err
		}
		if i == 0 {
			result.Changes.First = seq
		}
		result.Changes.Last = seq
	}
	result.Changes.Count = int64(len(result.Ids))
	countOwner := ""
	if state.schema.HasOwner {
		countOwner = opts.Owner
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO _dolmen_lakehouse_counts(table_name, generation, owner, n) VALUES(?,?,?,?) ON CONFLICT(table_name, generation, owner) DO UPDATE SET n = n + excluded.n`, inc.Table, inc.DropGen, countOwner, len(rows)); err != nil {
		return result, err
	}
	if opts.IdempotencyKey != "" {
		raw, err := json.Marshal(result.Ids)
		if err != nil {
			return result, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO _dolmen_lakehouse_idempotency(table_name, generation, owner, key, payload_hash, result) VALUES(?,?,?,?,?,?)`, inc.Table, inc.DropGen, domain.Owner, opts.IdempotencyKey, hash.Primary, string(raw)); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (s *Store) materialize(ctx context.Context, n *namespace, ns string) error {
	if n.pending == 0 {
		return nil
	}
	defer s.wake.signal(ns)
	rows, err := n.db.QueryContext(ctx, `SELECT commit_id, table_name, generation, rows FROM _dolmen_lakehouse_commits WHERE materialized = 0 ORDER BY commit_id`)
	if err != nil {
		return err
	}
	type pending struct {
		id         int64
		table      string
		generation int64
		payload    []byte
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.table, &p.generation, &p.payload); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, p)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, p := range todo {
		if err := s.materializeCommit(ctx, n, ns, p.id, p.table, p.generation, p.payload); err != nil {
			return fmt.Errorf("materialize lakehouse commit %d into %s: %w", p.id, p.table, err)
		}
		if _, err := n.db.ExecContext(ctx, `UPDATE _dolmen_lakehouse_commits SET materialized = 1, rows = x'' WHERE commit_id = ?`, p.id); err != nil {
			return err
		}
		n.pending--
	}
	n.pending = 0
	return nil
}

func (s *Store) materializeCommit(ctx context.Context, n *namespace, ns string, id int64, name string, generation int64, payload []byte) error {
	state, err := loadTable(ctx, n, ns, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if state.incarnation.DropGen != generation {
		return nil
	}
	marker := strconv.FormatInt(id, 10)
	for _, snap := range state.native.Metadata().Snapshots() {
		if snap.Summary != nil && snap.Summary.Properties[commitProperty] == marker {
			return nil
		}
	}
	if materializeHook != nil {
		if err := materializeHook(); err != nil {
			return err
		}
	}
	var decoded commitRows
	if err := gob.NewDecoder(bytes.NewReader(payload)).Decode(&decoded); err != nil {
		return fmt.Errorf("%w: unreadable lakehouse commit payload: %v", store.ErrCatalogCorrupt, err)
	}
	tx := state.native.NewTransaction()
	if len(decoded.Delete) > 0 && state.native.Metadata().CurrentSnapshot() != nil {
		if state.native.Properties()[table.WriteDeleteModeKey] != table.WriteModeMergeOnRead {
			if err := tx.SetProperties(iceberg.Properties{table.WriteDeleteModeKey: table.WriteModeMergeOnRead}); err != nil {
				return err
			}
		}
		if err := tx.Delete(ctx, idFilter(decoded.Delete), iceberg.Properties{commitProperty: marker}); err != nil {
			return err
		}
	}
	if len(decoded.Rows) > 0 {
		path, err := s.writeDataFile(state.native, id, decoded.Rows)
		if err != nil {
			return err
		}
		if err := tx.AddFiles(ctx, []string{fileLocation(path)}, iceberg.Properties{commitProperty: marker}, false); err != nil {
			return err
		}
	}
	native, err := tx.Commit(ctx)
	if err != nil {
		return err
	}
	return s.syncMetadata(native, n)
}

type plainWriter struct{ io.Writer }

func (s *Store) writeDataFile(native *table.Table, id int64, rows []map[string]any) (string, error) {
	sch, err := table.SchemaToArrowSchema(native.Schema(), nil, true, false)
	if err != nil {
		return "", err
	}
	record, err := arrowRecord(sch, rows)
	if err != nil {
		return "", err
	}
	defer record.Release()
	dir := filepath.Join(filepath.FromSlash(strings.TrimPrefix(native.Location(), "file://")), "data")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("commit-%020d-%s.parquet", id, uuid.NewString()))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	w, err := pqarrow.NewFileWriter(sch, plainWriter{f}, parquet.NewWriterProperties(), pqarrow.DefaultWriterProps())
	if err == nil {
		err = w.Write(record)
		err = errors.Join(err, w.Close())
	}
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		os.Remove(path)
		return "", err
	}
	rel, err := filepath.Rel(s.dir, dir)
	if err != nil {
		return "", err
	}
	if err := s.syncDirectory(rel); err != nil {
		return "", err
	}
	return path, nil
}

func arrowRecord(sch *arrow.Schema, rows []map[string]any) (arrow.Record, error) {
	b := array.NewRecordBuilder(memory.NewGoAllocator(), sch)
	defer b.Release()
	for col, field := range sch.Fields() {
		fb := b.Field(col)
		for _, row := range rows {
			v, ok := row[field.Name]
			if !ok || v == nil {
				if !field.Nullable {
					return nil, fmt.Errorf("lakehouse column %s is required but a row has no value", field.Name)
				}
				fb.AppendNull()
				continue
			}
			var err error
			switch builder := fb.(type) {
			case *array.Int64Builder:
				x, ok := v.(int64)
				if !ok {
					err = fmt.Errorf("lakehouse column %s holds %T, not an integer", field.Name, v)
				}
				builder.Append(x)
			case *array.BooleanBuilder:
				x, ok := v.(bool)
				if !ok {
					err = fmt.Errorf("lakehouse column %s holds %T, not a boolean", field.Name, v)
				}
				builder.Append(x)
			case *array.StringBuilder:
				x, ok := v.(string)
				if !ok {
					err = fmt.Errorf("lakehouse column %s holds %T, not text", field.Name, v)
				}
				builder.Append(x)
			case *array.BinaryBuilder:
				x, ok := v.([]byte)
				if !ok {
					err = fmt.Errorf("lakehouse column %s holds %T, not bytes", field.Name, v)
				}
				builder.Append(x)
			default:
				err = fmt.Errorf("lakehouse column %s has unsupported arrow type %s", field.Name, field.Type)
			}
			if err != nil {
				return nil, err
			}
		}
	}
	return b.NewRecord(), nil
}

func rowCount(ctx context.Context, db *sql.DB, inc store.Incarnation, scope *store.RowScope) (int64, error) {
	if scope != nil && scope.Empty {
		return 0, nil
	}
	var n sql.NullInt64
	var err error
	if scope != nil && scope.Owner != "" {
		err = db.QueryRowContext(ctx, `SELECT n FROM _dolmen_lakehouse_counts WHERE table_name=? AND generation=? AND owner=?`, inc.Table, inc.DropGen, scope.Owner).Scan(&n)
	} else {
		err = db.QueryRowContext(ctx, `SELECT sum(n) FROM _dolmen_lakehouse_counts WHERE table_name=? AND generation=?`, inc.Table, inc.DropGen).Scan(&n)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n.Int64, err
}

func (s *Store) publishSchema(ctx context.Context, n *namespace, state tableState) error {
	raw, err := json.Marshal(state.schema)
	if err != nil {
		return err
	}
	tx := state.native.NewTransaction()
	if err := tx.SetProperties(iceberg.Properties{schemaProperty: string(raw)}); err != nil {
		return err
	}
	native, err := tx.Commit(ctx)
	if err != nil {
		return err
	}
	return s.syncMetadata(native, n)
}
