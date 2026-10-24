package lakehouse

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func (s *Store) ImportRecords(ctx context.Context, ns, name string, records []map[string]any, opts store.WriteOpts, emb store.Embedder, expected store.Incarnation) (store.ImportResult, error) {
	identities, clean, err := store.ImportIdentities(records, opts.IdempotencyKey)
	if err != nil {
		return store.ImportResult{}, err
	}
	if s.tracer != nil {
		var span trace.Span
		ctx, span = s.tracer.Start(ctx, "IMPORT "+name, trace.WithAttributes(attribute.String("db.operation.name", "IMPORT")))
		defer span.End()
	}
	normalized := make([]map[string]any, len(clean))
	for i, row := range clean {
		normalized[i] = maps.Clone(row)
		normalized[i]["id"], normalized[i]["created_at"] = identities[i].ID, identities[i].CreatedAt
	}
	var result store.ImportResult
	err = s.withNamespace(ctx, ns, func(n *namespace) error {
		state, err := loadTable(ctx, n, ns, name)
		if err != nil {
			return err
		}
		if err := checkExpected(state, expected, false); err != nil {
			return err
		}
		if err := store.RequireSecretKey(s.secrets, state.schema.Fields); err != nil {
			return err
		}
		hash, err := store.RequestHash(ctx, s.secrets, state.schema, normalized)
		if err != nil {
			return err
		}
		if found, err := importReceipt(ctx, n.db, state, opts, hash, &result); err != nil || found {
			return err
		}
		for i, rec := range clean {
			for _, f := range state.schema.Fields {
				if _, present := rec[f.Name]; !present && f.Type == schema.Timestamp && schema.IsNowDefault(f.Default) {
					rec[f.Name] = identities[i].CreatedAt
				}
			}
		}
		stamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
		space, dim := state.schema.EmbedSpace, state.schema.EmbedDim
		rows, err := s.prepareRows(ctx, ns, state, clean, emb, stamp, opts.Owner)
		var at *recordError
		if errors.As(err, &at) {
			return fmt.Errorf("import record %d: %w", at.index, at.error)
		}
		if err != nil {
			return err
		}
		pin := state.schema.EmbedDim != dim || space == "" && state.schema.EmbedDim != 0
		ids := make([]int64, len(rows))
		for i, row := range rows {
			ids[i] = identities[i].ID
			row["id"], row["created_at"] = identities[i].ID, identities[i].CreatedAt
		}
		pending, err := s.skipIdentical(ctx, n, state, ids, rows)
		if err != nil {
			return err
		}
		result = store.ImportResult{Ids: ids, Skipped: int64(len(rows) - len(pending))}
		if err := s.checkAppend(ctx, n, state, pending); err != nil {
			return err
		}
		tx, err := n.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if len(pending) > 0 {
			pinned := commitRows{}
			if pin {
				pinned = commitRows{EmbedSpace: emb.Identity, EmbedDim: state.schema.EmbedDim}
			}
			committed, err := s.commitAppend(ctx, tx, state, pending, store.WriteOpts{Owner: opts.Owner}, store.IdemDomain{}, store.IdemHash{}, stamp, pinned)
			if err != nil {
				return err
			}
			result.Inserted, result.Changes = int64(len(pending)), committed.Changes
		}
		raw, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO _dolmen_lakehouse_imports(table_name, generation, owner, key, payload_hash, result) VALUES(?,?,?,?,?,?)`, state.incarnation.Table, state.incarnation.DropGen, opts.Owner, opts.IdempotencyKey, hash.Primary, string(raw)); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if len(pending) > 0 {
			n.pending++
			s.materialize(ctx, n, ns)
		}
		return nil
	})
	if err != nil {
		return store.ImportResult{}, err
	}
	return result, nil
}

func importReceipt(ctx context.Context, db *sql.DB, state tableState, opts store.WriteOpts, hash store.IdemHash, result *store.ImportResult) (bool, error) {
	var stored, raw string
	err := db.QueryRowContext(ctx, `SELECT payload_hash, result FROM _dolmen_lakehouse_imports WHERE table_name=? AND generation=? AND owner=? AND key=?`, state.incarnation.Table, state.incarnation.DropGen, opts.Owner, opts.IdempotencyKey).Scan(&stored, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !hash.Matches(stored) {
		return false, derr.New(derr.Conflict, "import idempotency key was used with different content; retry the same payload or use a new key")
	}
	if err := json.Unmarshal([]byte(raw), result); err != nil {
		return false, fmt.Errorf("%w: corrupt lakehouse import receipt: %v", store.ErrCatalogCorrupt, err)
	}
	result.Replayed = true
	return true, nil
}

func (s *Store) skipIdentical(ctx context.Context, n *namespace, state tableState, ids []int64, rows []map[string]any) ([]map[string]any, error) {
	existing, err := s.currentRows(ctx, state, ids)
	if err != nil || len(existing) == 0 {
		return rows, err
	}
	reveal := map[string]bool{}
	for _, f := range state.schema.SecretFields() {
		reveal[f.Name] = true
	}
	sealed, err := loadSecrets(ctx, n, state, reveal, mapValues(existing))
	if err != nil {
		return nil, err
	}
	var pending []map[string]any
	for i, row := range rows {
		current, ok := existing[ids[i]]
		if !ok {
			pending = append(pending, row)
			continue
		}
		candidate, own := maps.Clone(row), map[string][]byte{}
		for f := range reveal {
			if b, ok := row[f].([]byte); ok {
				own[f], candidate[f] = b, secretPresent
			}
		}
		a, _, err := s.presentRow(state.schema, reveal, sealed[ids[i]], current, false)
		if err != nil {
			return nil, err
		}
		b, _, err := s.presentRow(state.schema, reveal, own, candidate, false)
		if err != nil {
			return nil, err
		}
		delete(a, schema.OwnerColumn)
		delete(b, schema.OwnerColumn)
		x, _ := json.Marshal(a)
		y, _ := json.Marshal(b)
		if string(x) != string(y) {
			return nil, derr.New(derr.Conflict, "import record %d differs from the existing id; use a fresh target or correct the source", i)
		}
	}
	return pending, nil
}
