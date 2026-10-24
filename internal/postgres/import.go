package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func (s *Store) ImportRecords(ctx context.Context, ns, table string, records []map[string]any, opts store.WriteOpts, emb store.Embedder, expected store.Incarnation) (store.ImportResult, error) {
	ctx, end := s.span(ctx, "IMPORT", ns, table)
	res, err := s.importRows(ctx, ns, table, records, opts, emb, expected)
	end(err)
	return res, err
}

func (s *Store) importRows(ctx context.Context, ns, table string, records []map[string]any, opts store.WriteOpts, emb store.Embedder, expected store.Incarnation) (store.ImportResult, error) {
	identities, clean, err := store.ImportIdentities(records, opts.IdempotencyKey)
	if err != nil {
		return store.ImportResult{}, err
	}
	normalized := make([]map[string]any, len(clean))
	for i, row := range clean {
		normalized[i] = maps.Clone(row)
		normalized[i]["id"], normalized[i]["created_at"] = identities[i].ID, identities[i].CreatedAt
	}
	var state tableState
	var hash store.IdemHash
	var result store.ImportResult
	found := false
	err = s.read(ctx, ns, func(tx pgx.Tx, n namespace) error {
		var err error
		if state, err = s.loadTable(ctx, tx, n, table); err != nil {
			return err
		}
		if err := s.guardScope(ctx, tx, n, table, state, expected); err != nil {
			return err
		}
		if hash, err = s.recordHash(ctx, state.schema, normalized); err != nil {
			return err
		}
		result, found, err = s.importReceipt(ctx, tx, n, state, opts, hash)
		return err
	})
	if err != nil || found {
		return result, err
	}
	before, _ := json.Marshal(state.schema)
	rows, err := s.prepareRows(ctx, ns, state, clean, emb)
	var at *recordError
	if errors.As(err, &at) {
		return store.ImportResult{}, fmt.Errorf("import record %d: %w", at.index, at.error)
	}
	if err != nil {
		return store.ImportResult{}, err
	}
	ids := make([]int64, len(rows))
	for i, row := range rows {
		ids[i] = identities[i].ID
		for j, v := range row.values {
			if _, ok := v.(currentTimestamp); ok {
				row.values[j] = identities[i].CreatedAt
			}
		}
		rows[i].columns = append(row.columns, ident("id"), ident("created_at"))
		rows[i].values = append(row.values, identities[i].ID, identities[i].CreatedAt)
	}
	err = s.write(ctx, ns, state.incarnation.NsGen, func(tx pgx.Tx, n namespace) error {
		current, err := s.loadTable(ctx, tx, n, table)
		if err != nil {
			return err
		}
		if err := s.guardScope(ctx, tx, n, table, current, expected); err != nil {
			return err
		}
		if err := checkSameTable(ns, current.incarnation, state.incarnation); err != nil {
			return err
		}
		if result, found, err = s.importReceipt(ctx, tx, n, current, opts, hash); err != nil || found {
			return err
		}
		if now, _ := json.Marshal(current.schema); string(now) != string(before) {
			return fmt.Errorf("%w: table schema changed concurrently; retry the import", store.ErrInvalid)
		}
		if opts.Owner != "" && current.schema.HasOwner {
			for i := range rows {
				rows[i].columns = append(rows[i].columns, ident(schema.OwnerColumn))
				rows[i].values = append(rows[i].values, opts.Owner)
			}
		}
		pending, err := s.skipIdentical(ctx, tx, n, current, ids, rows)
		if err != nil {
			return err
		}
		result = store.ImportResult{Ids: ids, Skipped: int64(len(rows) - len(pending))}
		var inserted []int64
		var top int64
		for _, i := range pending {
			if err := s.insertImported(ctx, tx, ident(n.physical, current.physical), rows[i]); err != nil {
				return err
			}
			inserted = append(inserted, ids[i])
			top = max(top, ids[i])
		}
		if len(inserted) == 0 {
			return s.recordImport(ctx, tx, n, current, opts, hash, result)
		}
		var seq string
		if err := tx.QueryRow(ctx, "SELECT pg_get_serial_sequence($1,'id')", ident(n.physical, current.physical)).Scan(&seq); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SELECT setval($1::regclass, GREATEST(last_value, $2)) FROM "+seq, seq, top); err != nil {
			return err
		}
		if after, _ := json.Marshal(state.schema); string(after) != string(before) {
			if after, err = s.keepUnknownKeys(ctx, tx, ns, table, after, nil); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "UPDATE "+s.relation("tables")+" SET schema_json=$1 WHERE namespace=$2 AND name=$3", string(after), ns, table); err != nil {
				return err
			}
		}
		result.Inserted = int64(len(inserted))
		if result.Changes, err = s.mintChanges(ctx, tx, n, current, store.ChangeInsert, inserted, sameOwner(current.schema, opts.Owner, len(inserted))); err != nil {
			return err
		}
		return s.recordImport(ctx, tx, n, current, opts, hash, result)
	})
	if err != nil {
		return store.ImportResult{}, err
	}
	return result, nil
}

func (s *Store) insertImported(ctx context.Context, tx pgx.Tx, relation string, row preparedRow) error {
	params := make([]string, len(row.values))
	for i := range params {
		params[i] = "$" + strconv.Itoa(i+1)
	}
	_, err := tx.Exec(ctx, "INSERT INTO "+relation+" ("+strings.Join(row.columns, ",")+") OVERRIDING SYSTEM VALUE VALUES ("+strings.Join(params, ",")+")", row.values...)
	return err
}

func (s *Store) skipIdentical(ctx context.Context, tx pgx.Tx, n namespace, state tableState, ids []int64, rows []preparedRow) ([]int, error) {
	reveal := map[string]bool{}
	for _, f := range state.schema.SecretFields() {
		reveal[f.Name] = true
	}
	existing, truncated, err := s.selectRows(ctx, tx, n, state, rowSelection{ids: ids}, nil, reveal)
	if truncated || errors.Is(err, store.ErrInvalid) {
		return nil, fmt.Errorf("%w: existing import row exceeds the read budget; reduce its size before importing", store.ErrInvalid)
	}
	if err != nil {
		return nil, err
	}
	present := map[int64]bool{}
	for _, row := range existing {
		present[row["id"].(int64)] = true
	}
	var pending, overlap []int
	for i, id := range ids {
		if present[id] {
			overlap = append(overlap, i)
		} else {
			pending = append(pending, i)
		}
	}
	if len(overlap) == 0 {
		return pending, nil
	}
	const scratch = "dolmen_import_compare"
	if _, err := tx.Exec(ctx, "CREATE TEMP TABLE "+scratch+" (LIKE "+ident(n.physical, state.physical)+") ON COMMIT DROP"); err != nil {
		return nil, err
	}
	for _, i := range overlap {
		if err := s.insertImported(ctx, tx, scratch, rows[i]); err != nil {
			return nil, err
		}
	}
	candidates, truncated, err := s.selectRows(ctx, tx, namespace{physical: "pg_temp"}, tableState{schema: state.schema, columns: state.columns, physical: scratch}, rowSelection{ids: ids}, nil, reveal)
	if truncated || errors.Is(err, store.ErrInvalid) {
		return nil, fmt.Errorf("%w: existing import row exceeds the read budget; reduce its size before importing", store.ErrInvalid)
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, "DROP TABLE "+scratch); err != nil {
		return nil, err
	}
	index := map[int64]int{}
	for i, id := range ids {
		index[id] = i
	}
	for k, row := range candidates {
		if comparable(state, row) != comparable(state, existing[k]) {
			return nil, derr.New(derr.Conflict, "import record %d differs from the existing id; use a fresh target or correct the source", index[row["id"].(int64)])
		}
	}
	return pending, nil
}

func comparable(state tableState, row map[string]any) string {
	view := map[string]any{"id": row["id"], "created_at": row["created_at"]}
	for _, f := range state.schema.Fields {
		view[f.Name] = row[f.Name]
	}
	raw, _ := json.Marshal(view)
	return string(raw)
}

func (s *Store) importReceipt(ctx context.Context, tx pgx.Tx, n namespace, state tableState, opts store.WriteOpts, hash store.IdemHash) (store.ImportResult, bool, error) {
	var result store.ImportResult
	var stored, raw string
	err := tx.QueryRow(ctx, "SELECT payload_hash,result_json FROM "+s.relation("imports")+" WHERE namespace=$1 AND table_name=$2 AND drop_generation=$3 AND owner=$4 AND key=$5", n.name, state.incarnation.Table, state.incarnation.DropGen, opts.Owner, opts.IdempotencyKey).Scan(&stored, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, false, nil
	}
	if err != nil {
		return result, false, err
	}
	if !hash.Matches(stored) {
		return result, false, derr.New(derr.Conflict, "import idempotency key was used with different content; retry the same payload or use a new key")
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return result, false, fmt.Errorf("postgres: corrupt import receipt: %w", err)
	}
	result.Replayed = true
	return result, true, nil
}

func (s *Store) recordImport(ctx context.Context, tx pgx.Tx, n namespace, state tableState, opts store.WriteOpts, hash store.IdemHash, result store.ImportResult) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "INSERT INTO "+s.relation("imports")+" (namespace,table_name,drop_generation,owner,key,payload_hash,result_json) VALUES($1,$2,$3,$4,$5,$6,$7)", n.name, state.incarnation.Table, state.incarnation.DropGen, opts.Owner, opts.IdempotencyKey, hash.Primary, string(raw))
	return err
}
