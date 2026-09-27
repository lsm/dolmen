package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
)

const embedBackfillPage = 128

const maxMigrateAttempts = 3

var errMigrateRetry = errors.New("migration must retry activation")

type embedWork struct {
	embedding bool
	clear     bool
	addColumn bool
	target    string
	source    string
	hasSource bool
	constant  string
}

type stagedPlan struct {
	work *migrationWork
	gen  int64
}

func (s *Store) beginMigrate(nsName, table string) func() {
	s.migrateMu.Lock()
	defer s.migrateMu.Unlock()
	if s.migrating == nil {
		s.migrating = map[string]int{}
	}
	s.migrating[nsName+"."+table]++
	return func() {
		s.migrateMu.Lock()
		defer s.migrateMu.Unlock()
		if s.migrating == nil {
			return
		}
		key := nsName + "." + table
		if s.migrating[key] <= 1 {
			delete(s.migrating, key)
			return
		}
		s.migrating[key]--
	}
}

func (s *Store) migrateRunning(nsName, table string) bool {
	s.migrateMu.Lock()
	defer s.migrateMu.Unlock()
	return s.migrating[nsName+"."+table] > 0
}

func (s *Store) readMigratePlan(ctx context.Context, n *nsDB, nsName, table string, changes []schema.Change, emb Embedder, expected Incarnation) (*stagedPlan, error) {
	tx, err := n.ro.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := checkBoundLifetime(ctx, tx, nsName, table, expected); err != nil {
		return nil, err
	}
	old, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return nil, err
	}
	if err := checkExpectedVersion(nsName, table, int(expected.Version), old); err != nil {
		return nil, err
	}
	w, err := planMigration(ctx, tx, nsName, table, old, changes, emb, int(expected.Version), nil)
	if err != nil {
		return nil, err
	}
	gen, err := tableGen(ctx, tx, table)
	if err != nil {
		return nil, err
	}
	return &stagedPlan{work: w, gen: gen}, nil
}

func (s *Store) stageBackfill(ctx context.Context, n *nsDB, nsName, table string, plan *stagedPlan, emb Embedder) ([]float32, error) {
	e := &plan.work.embed
	if e.constant != "" {
		vecs, err := EmbedTexts(ctx, plan.work.cur, table, []string{e.constant}, emb)
		if err != nil {
			return nil, err
		}
		return vecs[0], nil
	}
	if !e.hasSource {
		return nil, nil
	}
	var after int64
	staged := int64(0)
	var reported int64
	for {
		ids, texts, err := s.backfillPage(ctx, n, table, e.source, after)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return nil, nil
		}
		stagedNow, err := loadStagedVectors(ctx, n.ro, table, plan.gen, emb.Identity, ids)
		if err != nil {
			return nil, err
		}
		var batch []stagedVector
		var fresh []string
		for i, id := range ids {
			digest := textDigest(texts[i])
			if held, ok := stagedNow[id]; ok && held.digest == digest {
				staged++
				continue
			}
			batch = append(batch, stagedVector{id: id, digest: digest})
			fresh = append(fresh, texts[i])
		}
		if len(batch) > 0 {
			vecs, err := EmbedTexts(ctx, plan.work.cur, table, fresh, emb)
			if err != nil {
				return nil, err
			}
			if err := checkBackfillVectors(table, vecs, plan.work); err != nil {
				return nil, err
			}
			for i := range batch {
				batch[i].vec = vecs[i]
			}
			if err := s.stagePage(ctx, n, table, plan.gen, emb.Identity, batch); err != nil {
				return nil, err
			}
			staged += int64(len(batch))
		}
		after = ids[len(ids)-1]
		reported = reportBackfill(nsName, table, staged, plan.work.plan.EmbedRows, reported)
	}
}

func reportBackfill(nsName, table string, staged, total, reported int64) int64 {
	if total <= 0 {
		return reported
	}
	tenth := total / 10
	if tenth < 1 {
		tenth = 1
	}
	if staged/tenth <= reported {
		return reported
	}
	slog.Info("migration: embedding backfill", "namespace", nsName, "table", table, "staged", staged, "total", total)
	return staged / tenth
}

func (s *Store) backfillPage(ctx context.Context, n *nsDB, table, column string, after int64) ([]int64, []string, error) {
	tx, err := n.ro.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(
		`SELECT id, %s FROM %s WHERE %s IS NOT NULL AND %s != '' AND id > ? ORDER BY id LIMIT %d`,
		q(column), q(table), q(column), q(column), embedBackfillPage), after)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var ids []int64
	var texts []string
	for rows.Next() {
		var id int64
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
		texts = append(texts, text)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return ids, texts, nil
}

func (s *Store) stagePage(ctx context.Context, n *nsDB, table string, gen int64, provider string, batch []stagedVector) error {
	ctx, tx, txSpan, err := s.beginWrite(ctx, n)
	if err != nil {
		return err
	}
	defer s.endWrite(tx, txSpan)
	if err := writeStagedVectors(ctx, tx, table, gen, provider, batch); err != nil {
		return err
	}
	return commitWrite(tx, txSpan)
}

func checkBackfillVectors(table string, vecs [][]float32, w *migrationWork) error {
	for _, v := range vecs {
		if len(v) == 0 {
			return invalidf("backfill: embedding provider returned a zero-dimensional vector for table %s", table)
		}
		for _, x := range v {
			if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
				return invalidf("backfill: embedding provider returned a non-finite vector component for table %s", table)
			}
		}
		if w.cur.EmbedDim == 0 {
			w.cur.EmbedDim = len(v)
		} else if len(v) != w.cur.EmbedDim {
			return invalidf("embedding provider returned %d-dimensional vectors mid-backfill (expected %d)", len(v), w.cur.EmbedDim)
		}
	}
	return nil
}

func (s *Store) coverageCheck(ctx context.Context, n *nsDB, nsName, table string, plan *stagedPlan, provider string) (int64, error) {
	e := &plan.work.embed
	if !e.embedding {
		return 0, nil
	}
	tx, err := n.ro.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	head, err := changeHead(ctx, tx)
	if err != nil {
		return 0, err
	}
	if e.constant != "" || e.source == "" {
		return head, nil
	}
	var after int64
	for {
		ids, texts, err := s.backfillPage(ctx, n, table, e.source, after)
		if err != nil {
			return 0, err
		}
		if len(ids) == 0 {
			return head, nil
		}
		staged, err := loadStagedVectors(ctx, tx, table, plan.gen, provider, ids)
		if err != nil {
			return 0, err
		}
		for i, id := range ids {
			held, ok := staged[id]
			if !ok || held.digest != textDigest(texts[i]) {
				return 0, errMigrateRetry
			}
		}
		after = ids[len(ids)-1]
	}
}

func (s *Store) rowsStaleSince(ctx context.Context, tx *sql.Tx, table string, gen int64, provider, source string, from, to int64) (bool, error) {
	if to <= from {
		return false, nil
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT row_id FROM _dolmen_changes WHERE table_name = ? AND drop_gen = ? AND seq > ? AND seq <= ?`,
		table, gen, from, to)
	if err != nil {
		return false, err
	}
	var touched []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		touched = append(touched, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(touched) == 0 {
		return false, nil
	}
	staged, err := loadStagedVectors(ctx, tx, table, gen, provider, touched)
	if err != nil {
		return false, err
	}
	return s.anyRowMissing(ctx, tx, table, source, staged, touched)
}

func (s *Store) anyRowMissing(ctx context.Context, tx *sql.Tx, table, column string, staged map[int64]stagedVector, ids []int64) (bool, error) {
	for _, id := range ids {
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT id, %s FROM %s WHERE id = ?`, q(column), q(table)), id)
		if err != nil {
			return false, err
		}
		var gotID int64
		var text sql.NullString
		found := rows.Next()
		if found {
			if err := rows.Scan(&gotID, &text); err != nil {
				rows.Close()
				return false, err
			}
		}
		rows.Close()
		if !found {
			continue
		}
		if !text.Valid || text.String == "" {
			continue
		}
		held, ok := staged[gotID]
		if !ok || held.digest != textDigest(text.String) {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) activateMigration(ctx context.Context, n *nsDB, nsName, table string, changes []schema.Change, emb Embedder, expected Incarnation, plan *stagedPlan, constant []float32, head int64) (*schema.TableSchema, bool, error) {
	ctx, tx, txSpan, err := s.beginWrite(ctx, n)
	if err != nil {
		return nil, false, err
	}
	defer s.endWrite(tx, txSpan)

	if err := checkBoundLifetime(ctx, tx, nsName, table, expected); err != nil {
		return nil, false, err
	}
	old, err := loadSchema(ctx, tx, nsName, table)
	if err != nil {
		return nil, false, err
	}
	if err := checkExpectedVersion(nsName, table, int(expected.Version), old); err != nil {
		return nil, false, err
	}
	gen, err := tableGen(ctx, tx, table)
	if err != nil {
		return nil, false, err
	}
	w, err := planMigration(ctx, tx, nsName, table, old, changes, emb, int(expected.Version), nil)
	if err != nil {
		return nil, false, err
	}
	e := &w.embed
	if e.embedding && e.hasSource {
		cur, err := changeHead(ctx, tx)
		if err != nil {
			return nil, false, err
		}
		stale, err := s.rowsStaleSince(ctx, tx, table, gen, emb.Identity, e.source, head, cur)
		if err != nil {
			return nil, false, err
		}
		if stale {
			return nil, true, nil
		}
	}
	if e.addColumn {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN "_embedding" BLOB`, q(table))); err != nil {
			if !strings.Contains(err.Error(), "duplicate column") {
				return nil, false, fmt.Errorf("add _embedding column: %w", err)
			}
		}
	}
	if e.clear {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET "_embedding" = NULL`, q(table))); err != nil {
			return nil, false, err
		}
	}
	if e.embedding && e.hasSource {
		if err := s.stampStagedVectors(ctx, tx, table, w, gen, emb.Identity, nil); err != nil {
			if errors.Is(err, errMigrateRetry) {
				return nil, true, nil
			}
			return nil, false, err
		}
	}
	for i, st := range w.steps {
		if err := s.migrateStep(ctx, "change", i, func(ctx context.Context) error { return st(ctx, tx) }); err != nil {
			return nil, false, fmt.Errorf("migration step failed: %w", err)
		}
	}
	if w.rebuildFTSNeeded {
		if err := s.migrateStep(ctx, "fts_rebuild", len(w.steps), func(ctx context.Context) error {
			if err := dropFTS(ctx, tx, table); err != nil {
				return err
			}
			if fts := ftsFields(w.cur.Fields); len(fts) > 0 {
				return createFTS(ctx, tx, table, fts)
			}
			return nil
		}); err != nil {
			return nil, false, err
		}
	}
	if e.embedding && e.constant != "" {
		if err := s.stampStagedVectors(ctx, tx, table, w, gen, emb.Identity, constant); err != nil {
			if errors.Is(err, errMigrateRetry) {
				return nil, true, nil
			}
			return nil, false, err
		}
	}
	if err := saveSchemaTx(ctx, tx, nsName, w.cur, old.Version, changes); err != nil {
		return nil, false, err
	}
	if err := deleteStagedVectors(ctx, tx, table); err != nil {
		return nil, false, err
	}
	if err := commitWrite(tx, txSpan); err != nil {
		return nil, false, err
	}
	return w.cur, false, nil
}

func (s *Store) stampStagedVectors(ctx context.Context, tx *sql.Tx, table string, w *migrationWork, gen int64, provider string, constant []float32) error {
	e := &w.embed
	if e.constant != "" {
		if constant == nil {
			return errMigrateRetry
		}
		res, err := tx.ExecContext(ctx, fmt.Sprintf(
			`UPDATE %s SET "_embedding" = ? WHERE %s IS NOT NULL AND %s != ''`, q(table), q(e.target), q(e.target)),
			encodeStageVector(constant))
		if err != nil {
			return err
		}
		if affected, err := res.RowsAffected(); err == nil && affected > 0 && w.cur.EmbedDim == 0 {
			w.cur.EmbedDim = len(constant)
		}
		return nil
	}
	if !e.hasSource {
		return nil
	}
	var after int64
	for {
		page, err := stagedSourcePage(ctx, tx, table, e.source, after)
		if err != nil {
			return err
		}
		if len(page.ids) == 0 {
			return nil
		}
		staged, err := loadStagedVectors(ctx, tx, table, gen, provider, page.ids)
		if err != nil {
			return err
		}
		for i, id := range page.ids {
			held, ok := staged[id]
			if !ok || held.digest != textDigest(page.texts[i]) {
				return errMigrateRetry
			}
			if w.cur.EmbedDim == 0 {
				w.cur.EmbedDim = len(held.vec)
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET "_embedding" = ? WHERE id = ?`, q(table)),
				encodeStageVector(held.vec), id); err != nil {
				return err
			}
		}
		after = page.ids[len(page.ids)-1]
	}
}

type sourcePage struct {
	ids   []int64
	texts []string
}

func stagedSourcePage(ctx context.Context, db querier, table, column string, after int64) (sourcePage, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(
		`SELECT id, %s FROM %s WHERE %s IS NOT NULL AND %s != '' AND id > ? ORDER BY id LIMIT %d`,
		q(column), q(table), q(column), q(column), embedBackfillPage), after)
	if err != nil {
		return sourcePage{}, err
	}
	defer rows.Close()
	var page sourcePage
	for rows.Next() {
		var id int64
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			return sourcePage{}, err
		}
		page.ids = append(page.ids, id)
		page.texts = append(page.texts, text)
	}
	return page, rows.Err()
}

func migrationConflict(nsName, table string) error {
	return derr.New(derr.Conflict, "migration of %s.%s kept losing a race with concurrent writes while backfilling embeddings; re-issue the same migrate to continue from the rows already embedded", nsName, table)
}
