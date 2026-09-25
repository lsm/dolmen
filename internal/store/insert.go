package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/value"
)

type EmbedFn func(ctx context.Context, texts []string) ([][]float32, error)

type Embedder struct {
	Embed    EmbedFn
	Identity string
}

const MaxRecordsPerInsert = 1000

const MaxIdempotencyKeyLen = 256

func (s *Store) Insert(ctx context.Context, nsName, table string, records []map[string]any, opts WriteOpts, emb Embedder, scope *RowScope, scopeIncarnation Incarnation) (InsertResult, error) {
	if err := s.guardIncarnation(ctx, nsName, table, scopeIncarnation); err != nil {
		return InsertResult{}, err
	}
	if len(opts.IdempotencyKey) > MaxIdempotencyKeyLen {
		return InsertResult{}, invalidf("idempotency key is %d bytes (max %d)", len(opts.IdempotencyKey), MaxIdempotencyKeyLen)
	}
	ids, changes, replayed, err := s.insert(ctx, nsName, table, records, emb, opts.IdempotencyKey, opts.Owner, DomainFor(opts, scope))
	if err != nil {
		return InsertResult{}, err
	}
	return InsertResult{Ids: ids, Replayed: replayed, Changes: changes}, nil
}

func (s *Store) insert(ctx context.Context, nsName, table string, records []map[string]any, emb Embedder, idemKey, owner string, domain IdemDomain) (ids []int64, changes ChangeRange, replayed bool, err error) {
	if len(records) == 0 {
		return nil, ChangeRange{}, false, invalidf("no records given")
	}
	if len(records) > MaxRecordsPerInsert {
		return nil, ChangeRange{}, false, invalidf("too many records: %d > %d per call", len(records), MaxRecordsPerInsert)
	}
	n, err := s.ns(nsName)
	if err != nil {
		return nil, ChangeRange{}, false, err
	}
	defer n.unpin()
	normalized := make([]map[string]any, len(records))
	for i, rec := range records {
		nr := make(map[string]any, len(rec))
		for k, v := range rec {
			lk := strings.ToLower(k)
			if _, exists := nr[lk]; exists {
				return nil, ChangeRange{}, false, invalidf("record %d: fields %q and its case variant collapse to %q; use one spelling", i, k, lk)
			}
			nr[lk] = v
		}
		normalized[i] = nr
	}
	records = normalized

	for attempt := 0; ; attempt++ {
		if attempt >= 3 {
			return nil, ChangeRange{}, false, invalidf("table schema changed concurrently; retry the insert")
		}
		ids, changes, replayed, done, err := s.insertAttempt(ctx, n, nsName, table, records, emb, idemKey, owner, domain)
		if done {
			return ids, changes, replayed, err
		}
	}
}

func (s *Store) payloadHash(sc *schema.TableSchema, records []map[string]any) string {
	hashed := make([]map[string]any, len(records))
	for i, rec := range records {
		hashed[i] = make(map[string]any, len(rec))
		for k, v := range rec {
			if f := sc.Field(k); f != nil && f.Type == schema.Secret && v != nil {
				v = s.secretFingerprint(v)
			}
			hashed[i][k] = v
		}
	}
	raw, err := json.Marshal(hashed)
	if err != nil {

		raw = []byte("marshal error: " + err.Error())
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (s *Store) insertAttempt(ctx context.Context, n *nsDB, nsName, table string, records []map[string]any, emb Embedder, idemKey, owner string, domain IdemDomain) (ids []int64, changes ChangeRange, replayed bool, done bool, err error) {

	gen, err := tableGen(ctx, n.rw, table)
	if err != nil {
		return nil, ChangeRange{}, false, true, err
	}
	sc, err := loadSchema(ctx, n.rw, nsName, table)
	if err != nil {
		return nil, ChangeRange{}, false, true, err
	}
	var idemHash string
	if idemKey != "" {
		idemHash = s.payloadHash(sc, records)
		if ids, found, err := lookupIdem(ctx, n.rw, table, idemKey, idemHash, domain); err != nil {
			return nil, ChangeRange{}, false, true, err
		} else if found {
			return ids, ChangeRange{}, true, true, nil
		}
	}
	persistMeta := sc.EmbedSpace == "" || sc.EmbedDim == 0
	origEmbedSpace := sc.EmbedSpace
	origEmbedDim := sc.EmbedDim

	records = applyInsertDefaults(sc, records)
	for _, rec := range records {
		for k := range rec {
			if sc.Field(k) == nil {
				return nil, ChangeRange{}, false, true, invalidf("unknown field %q on table %s (see describe_table)", k, table)
			}
		}
		for _, f := range sc.Fields {
			v, present := rec[f.Name]
			if present && v != nil {
				continue
			}
			if f.Required {
				return nil, ChangeRange{}, false, true, invalidf("field %q is required", f.Name)
			}
		}
	}

	type coercedRow struct {
		cols []string
		vals []any
		rec  map[string]any
	}
	coercedRows := make([]coercedRow, len(records))
	for i, rec := range records {
		var cols []string
		var vals []any
		for _, f := range sc.Fields {
			v, present := rec[f.Name]
			if !present {
				continue
			}
			cv, err := s.coerceWrite(f, v)
			if err != nil {
				return nil, ChangeRange{}, false, true, err
			}
			cols = append(cols, q(f.Name))
			vals = append(vals, cv)
		}
		coercedRows[i] = coercedRow{cols: cols, vals: vals, rec: rec}
	}

	embFor := map[int][]float32{}
	if vf := sc.VectorizeField(); vf != nil {
		var texts []string
		var idx []int
		for i, rec := range records {
			if t, ok := rec[vf.Name].(string); ok && t != "" {
				texts = append(texts, t)
				idx = append(idx, i)
			}
		}
		if len(texts) > 0 {
			vecs, err := embedTexts(ctx, sc, table, texts, emb)
			if err != nil {
				return nil, ChangeRange{}, false, true, err
			}
			for k, i := range idx {
				embFor[i] = vecs[k]
			}
		}
	}

	fts := sc.FTSFields()
	tx, err := n.rw.BeginTx(ctx, nil)
	if err != nil {
		return nil, ChangeRange{}, false, true, err
	}
	defer tx.Rollback()

	scTx, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return nil, ChangeRange{}, false, true, err
	}
	txGen, err := tableGen(ctx, tx, table)
	if err != nil {
		return nil, ChangeRange{}, false, true, err
	}
	if scTx.Version != sc.Version || scTx.EmbedSpace != origEmbedSpace || scTx.EmbedDim != origEmbedDim || txGen != gen {
		return nil, ChangeRange{}, false, false, nil
	}

	if idemKey != "" {
		ids, found, err := lookupIdem(ctx, tx, table, idemKey, idemHash, domain)
		if err != nil {
			return nil, ChangeRange{}, false, true, err
		}
		if found {
			return ids, ChangeRange{}, true, true, nil
		}
	}

	ids = make([]int64, 0, len(records))
	stmts := newStmtCache(tx)
	defer stmts.close()
	for i, row := range coercedRows {
		stampNowVals(row.vals)
		var vec []float32
		if ev, ok := embFor[i]; ok {
			vec = ev
		}
		id, err := execInsertWithFTS(ctx, stmts, table, fts, row.rec, row.cols, row.vals, vec, stampOwner(sc, owner))
		if err != nil {
			return nil, ChangeRange{}, false, true, err
		}
		ids = append(ids, id)
	}
	if len(embFor) > 0 && persistMeta {
		if sc.EmbedSpace == "" && emb.Identity != "" {
			sc.EmbedSpace = emb.Identity
		}
		if sc.EmbedDim == 0 {
			for _, v := range embFor {
				sc.EmbedDim = len(v)
				break
			}
		}
		raw, err := encodeSchemaOver(ctx, tx, table, sc, nil)
		if err != nil {
			return nil, ChangeRange{}, false, true, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE _dolmen_tables SET schema_json = ? WHERE name = ?`, raw, table); err != nil {
			return nil, ChangeRange{}, false, true, err
		}
	}

	changes, err = mintChanges(ctx, tx, table, ChangeInsert, ids, sameOwner(stampOwner(sc, owner), len(ids)))
	if err != nil {
		return nil, ChangeRange{}, false, true, err
	}
	if idemKey != "" {
		idsJSON, err := json.Marshal(ids)
		if err != nil {
			return nil, ChangeRange{}, false, true, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO `+idempotencyTable+`(table_name, owner, key, payload_hash, ids_json) VALUES(?,?,?,?,?)`,
			table, domain.Owner, idemKey, idemHash, string(idsJSON)); err != nil {

			if strings.Contains(err.Error(), "UNIQUE constraint failed: "+idempotencyTable) {
				if rerr := tx.Rollback(); rerr != nil {
					return nil, ChangeRange{}, false, true, rerr
				}
				ids, found, lerr := lookupIdem(ctx, n.rw, table, idemKey, idemHash, domain)
				if lerr != nil {
					return nil, ChangeRange{}, false, true, lerr
				}
				if !found {
					return nil, ChangeRange{}, false, true, invalidf("idempotency key %q vanished mid-insert; retry", idemKey)
				}
				return ids, ChangeRange{}, true, true, nil
			}
			return nil, ChangeRange{}, false, true, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, ChangeRange{}, false, true, err
	}

	s.notifyCommitted(nsName, table, changes)
	return ids, changes, false, true, nil
}

func applyInsertDefaults(sc *schema.TableSchema, records []map[string]any) []map[string]any {
	hasDefault := false
	for _, f := range sc.Fields {
		if f.Default != nil {
			hasDefault = true
			break
		}
	}
	if !hasDefault {
		return records
	}
	out := make([]map[string]any, len(records))
	for i, rec := range records {
		dr := make(map[string]any, len(rec)+1)
		for k, v := range rec {
			dr[k] = v
		}
		for _, f := range sc.Fields {
			if _, present := dr[f.Name]; !present && f.Default != nil {
				dr[f.Name] = writeDefault(f)
			}
		}
		out[i] = dr
	}
	return out
}

const nowStampLayout = "2006-01-02T15:04:05.000Z"

type nowStamp struct{}

func writeDefault(f schema.Field) any {
	if f.Type == schema.Timestamp && schema.IsNowDefault(f.Default) {
		return nowStamp{}
	}
	return f.Default
}

func stampNow(v any) any {
	if _, ok := v.(nowStamp); ok {
		return time.Now().UTC().Format(nowStampLayout)
	}
	return v
}

func stampNowVals(vals []any) {
	for j, v := range vals {
		vals[j] = stampNow(v)
	}
}

func defaultForWrite(f schema.Field) any {
	return stampNow(writeDefault(f))
}

func EmbedTexts(ctx context.Context, sc *schema.TableSchema, table string, texts []string, emb Embedder) ([][]float32, error) {
	return embedTexts(ctx, sc, table, texts, emb)
}

func embedTexts(ctx context.Context, sc *schema.TableSchema, table string, texts []string, emb Embedder) ([][]float32, error) {
	if emb.Embed == nil {
		return nil, invalidf("table %s uses vectorize but no embedding provider is configured", table)
	}
	if emb.Identity == "" {
		return nil, invalidf("table %s uses vectorize but the active embedding provider reports no identity; configure the provider before inserting so rows are attributable to an embedding space", table)
	}
	if sc.EmbedSpace != "" && sc.EmbedSpace != emb.Identity {
		return nil, invalidf("embedding provider changed: table rows were embedded by %q but the active provider is %q; re-embed via migrate (set_vectorize off, then on)", sc.EmbedSpace, emb.Identity)
	}
	vecs, err := emb.Embed(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("embedding failed: %w", err)
	}
	if len(vecs) != len(texts) {
		return nil, fmt.Errorf("embedding provider returned %d vectors for %d texts", len(vecs), len(texts))
	}
	for _, v := range vecs {
		if len(v) == 0 {
			return nil, invalidf("embedding provider returned a zero-dimensional vector for table %s", table)
		}
		for _, x := range v {
			if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
				return nil, invalidf("embedding provider returned a non-finite vector component for table %s", table)
			}
		}
		if sc.EmbedDim == 0 {
			sc.EmbedDim = len(v)
		} else if len(v) != sc.EmbedDim {
			return nil, invalidf("embedding provider returned %d-dimensional vectors but table %s stores %d-dimensional embeddings; re-embed via migrate (set_vectorize off, then on) if the provider changed", len(v), table, sc.EmbedDim)
		}
	}
	return vecs, nil
}

func execInsertWithFTS(ctx context.Context, tx stmtExecer, table string, fts []schema.Field, rec map[string]any, cols []string, vals []any, vec []float32, owner string) (int64, error) {
	if vec != nil {
		cols = append(cols, `"_embedding"`)
		vals = append(vals, schema.EncodeVector(vec))
	}
	if owner != "" {
		cols = append(cols, q(schema.OwnerColumn))
		vals = append(vals, owner)
	}
	var stmt string
	if len(cols) == 0 {
		stmt = fmt.Sprintf(`INSERT INTO %s DEFAULT VALUES`, q(table))
	} else {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",")
		stmt = fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`, q(table), strings.Join(cols, ", "), ph)
	}
	res, err := tx.ExecContext(ctx, stmt, vals...)
	if err != nil {
		return 0, fmt.Errorf("insert into %s: %w", table, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if len(fts) > 0 {
		if err := writeFTSRowFor(ctx, tx, table, fts, id, rec); err != nil {
			return 0, err
		}
	}
	return id, nil
}

func writeFTSRowFor(ctx context.Context, tx stmtExecer, table string, fts []schema.Field, id int64, rec map[string]any) error {
	fcols := make([]string, len(fts))
	fvals := make([]any, len(fts)+1)
	fvals[0] = id
	for j, f := range fts {
		fcols[j] = q(f.Name)
		fvals[j+1] = ftsText(rec[f.Name])
	}
	fph := strings.TrimSuffix(strings.Repeat("?,", len(fts)+1), ",")
	fstmt := fmt.Sprintf(`INSERT INTO %s (rowid, %s) VALUES (%s)`,
		q(ftsTable(table)), strings.Join(fcols, ", "), fph)
	if _, err := tx.ExecContext(ctx, fstmt, fvals...); err != nil {
		return fmt.Errorf("update search index for %s: %w", table, err)
	}
	return nil
}

func ftsText(v any) any {
	if v == nil {
		return nil
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func coerceValue(f schema.Field, v any) (any, error) {
	if _, ok := v.(nowStamp); ok {
		return v, nil
	}
	return value.Coerce(f, v)
}

func storedString(v any) (string, bool) {
	return value.StoredString(v)
}
