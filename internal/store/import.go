package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/lsm/dolmen/internal/schema"
)

type ImportResult struct {
	Ids      []int64
	Inserted int64
	Skipped  int64
	Replayed bool
	Changes  ChangeRange
}

type ImportIdentity struct {
	ID        int64
	CreatedAt string
}

func ImportIdentities(records []map[string]any, key string) ([]ImportIdentity, []map[string]any, error) {
	if len(records) < 1 || len(records) > MaxRecordsPerInsert {
		return nil, nil, invalidf("import requires 1–%d records; split the input into chunks", MaxRecordsPerInsert)
	}
	if len(key) < 1 || len(key) > MaxIdempotencyKeyLen || strings.IndexFunc(key, func(r rune) bool { return r < 32 || r > 126 }) >= 0 {
		return nil, nil, invalidf("import requires a printable ASCII idempotency key of 1–%d bytes", MaxIdempotencyKeyLen)
	}
	ids, clean, seen := make([]ImportIdentity, len(records)), make([]map[string]any, len(records)), map[int64]bool{}
	for i, rec := range records {
		clean[i] = map[string]any{}
		for k, v := range rec {
			name := strings.ToLower(k)
			if _, ok := clean[i][name]; ok {
				return nil, nil, invalidf("import record %d has duplicate case-insensitive fields", i)
			}
			clean[i][name] = v
		}
		var id int64
		switch v := clean[i]["id"].(type) {
		case json.Number:
			id, _ = strconv.ParseInt(string(v), 10, 64)
		case int64:
			id = v
		case int:
			id = int64(v)
		case float64:
			if v > 0 && v <= 9007199254740991 && v == math.Trunc(v) {
				id = int64(v)
			}
		}
		if id <= 0 || id == math.MaxInt64 || seen[id] {
			return nil, nil, invalidf("import record %d requires a distinct positive int64 id below MaxInt64; use an exact integer token", i)
		}
		stamp, _ := clean[i]["created_at"].(string)
		parsed, err := time.Parse(nowStampLayout, stamp)
		if err != nil || parsed.Format(nowStampLayout) != stamp {
			return nil, nil, invalidf("import record %d requires created_at in YYYY-MM-DDTHH:MM:SS.sssZ form; normalize in the export", i)
		}
		seen[id] = true
		ids[i] = ImportIdentity{id, stamp}
		delete(clean[i], "id")
		delete(clean[i], "created_at")
	}
	return ids, clean, nil
}

func (s *Store) ImportRecords(ctx context.Context, ns, table string, records []map[string]any, opts WriteOpts, emb Embedder, inc Incarnation) (ImportResult, error) {
	identities, clean, err := ImportIdentities(records, opts.IdempotencyKey)
	if err != nil {
		return ImportResult{}, err
	}
	n, err := s.ns(ns)
	if err != nil {
		return ImportResult{}, err
	}
	defer n.unpin()
	ctx, wt, err := s.writeTxFor(ctx, n, nil)
	if err != nil {
		return ImportResult{}, err
	}
	defer s.releaseWrite(wt)
	tx := wt.tx
	if err = checkScopeIncarnation(ctx, tx, ns, table, inc); err != nil {
		return ImportResult{}, err
	}
	sc, err := loadSchema(ctx, tx, ns, table)
	if err != nil {
		return ImportResult{}, err
	}
	gen, err := tableGen(ctx, tx, table)
	if err != nil {
		return ImportResult{}, err
	}
	normalized := make([]map[string]any, len(clean))
	for i, row := range clean {
		normalized[i] = maps.Clone(row)
		normalized[i]["id"], normalized[i]["created_at"] = identities[i].ID, identities[i].CreatedAt
	}
	hash, err := RequestHash(ctx, s.secrets, sc, normalized)
	if err != nil {
		return ImportResult{}, err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS _dolmen_imports(table_name TEXT NOT NULL, generation INTEGER NOT NULL, owner TEXT NOT NULL, key TEXT NOT NULL, payload_hash TEXT NOT NULL, result_json TEXT NOT NULL, PRIMARY KEY(table_name,generation,owner,key))`); err != nil {
		return ImportResult{}, err
	}
	var storedHash, raw string
	err = tx.QueryRowContext(ctx, `SELECT payload_hash,result_json FROM _dolmen_imports WHERE table_name=? AND generation=? AND owner=? AND key=?`, table, gen, opts.Owner, opts.IdempotencyKey).Scan(&storedHash, &raw)
	if err == nil {
		if !hash.Matches(storedHash) {
			return ImportResult{}, conflictf("import idempotency key was used with different content; retry the same payload or use a new key")
		}
		var result ImportResult
		if err = json.Unmarshal([]byte(raw), &result); err != nil {
			return ImportResult{}, err
		}
		result.Replayed = true
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ImportResult{}, err
	}
	proj := projectionFromSchema(sc, false)
	proj.secrets = s.secrets
	proj.reveal = map[string]bool{}
	for _, f := range sc.SecretFields() {
		proj.reveal[f.Name] = true
	}
	result := ImportResult{Ids: make([]int64, len(records))}
	var pending []map[string]any
	var pendingIDs []ImportIdentity
	for i, rec := range clean {
		result.Ids[i] = identities[i].ID
		prepared := applyInsertDefaults(sc, []map[string]any{rec})[0]
		expected := map[string]any{"id": identities[i].ID, "created_at": identities[i].CreatedAt}
		for name := range prepared {
			if sc.Field(name) == nil {
				return ImportResult{}, invalidf("import record %d has an unknown field; see describe_table", i)
			}
		}
		for _, f := range sc.Fields {
			v := prepared[f.Name]
			if _, now := v.(nowStamp); now {
				v = identities[i].CreatedAt
				prepared[f.Name] = v
			}
			if v == nil && f.Required {
				return ImportResult{}, invalidf("import record %d is missing a required field", i)
			}
			cv, e := s.coerceWrite(ctx, ns, f, v)
			if e != nil {
				if errors.Is(e, ErrInvalid) {
					return ImportResult{}, invalidf("import record %d field %s is invalid; check its declared type", i, f.Name)
				}
				return ImportResult{}, e
			}
			if f.Type == schema.Secret && cv != nil {
				cv, e = s.secrets.Open(cv.([]byte))
				if e != nil {
					return ImportResult{}, e
				}
				expected[f.Name] = cv
			} else {
				expected[f.Name] = decodeValue(f.Type, cv)
			}
			if f.Type != schema.File {
				prepared[f.Name] = expected[f.Name]
			}
		}
		existing, _, e := fetchByIDsScoped(ctx, tx, table, []int64{identities[i].ID}, proj, nil)
		var budget *responseBudgetError
		if errors.As(e, &budget) {
			return ImportResult{}, invalidf("existing import row exceeds the read budget; reduce its size before importing")
		}
		if e != nil {
			return ImportResult{}, e
		}
		if len(existing) > 0 {
			actual := map[string]any{"id": existing[0]["id"], "created_at": existing[0]["created_at"]}
			for _, f := range sc.Fields {
				actual[f.Name] = existing[0][f.Name]
			}
			a, _ := json.Marshal(actual)
			b, _ := json.Marshal(expected)
			if string(a) != string(b) {
				return ImportResult{}, conflictf("import record %d differs from the existing id; use a fresh target or correct the source", i)
			}
			result.Skipped++
			continue
		}
		pending = append(pending, prepared)
		pendingIDs = append(pendingIDs, identities[i])
	}
	if len(pending) > 0 {
		inner := &sharedWriteTx{tx: tx, ns: n}
		_, changes, _, done, e := s.insertAttempt(ctx, n, ns, table, pending, emb, "", opts.Owner, DomainFor(opts, nil), inner, pendingIDs)
		if e != nil {
			return ImportResult{}, e
		}
		if !done {
			return ImportResult{}, fmt.Errorf("import schema changed within its transaction")
		}
		result.Inserted = int64(len(pending))
		result.Changes = changes
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return ImportResult{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO _dolmen_imports VALUES(?,?,?,?,?,?)`, table, gen, opts.Owner, opts.IdempotencyKey, hash.Primary, string(encoded)); err != nil {
		return ImportResult{}, err
	}
	if err = s.commitOwned(wt); err != nil {
		return ImportResult{}, err
	}
	s.notifyCommitted(ns, table, result.Changes)
	return result, nil
}
