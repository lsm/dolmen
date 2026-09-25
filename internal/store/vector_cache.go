package store

import (
	"context"
	"database/sql"
	"fmt"
	"runtime"
	"strings"
	"sync"
)

const (
	DefaultVectorCacheBytes = 512 << 20
	parallelScoreRows       = 8192
	tooBigRetryChanges      = 1000
	vecRowOverhead          = 24 + 8 + 8 + 64
)

type vecKey struct {
	ns, table, column string
}

type vecFingerprint struct {
	nsGen   [16]byte
	dropGen int64
	version int
}

type vecEntry struct {
	mu      sync.RWMutex
	fp      vecFingerprint
	seq     int64
	pos     map[int64]int
	ids     []int64
	vecs    [][]float32
	sq      []float64
	bytes   int64
	lastUse uint64
	tooBig  bool
}

type vecCache struct {
	mu      sync.Mutex
	max     int64
	used    int64
	tick    uint64
	entries map[vecKey]*vecEntry
}

func (c *vecCache) entry(k vecKey) *vecEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[vecKey]*vecEntry{}
	}
	e, ok := c.entries[k]
	if !ok {
		e = &vecEntry{seq: -1}
		c.entries[k] = e
	}
	c.tick++
	e.lastUse = c.tick
	return e
}

func (c *vecCache) account(k vecKey, e *vecEntry, bytes int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.claim(k, e)
	c.used += bytes - e.bytes
	e.bytes = bytes
	if bytes > c.max {
		c.used -= bytes
		e.bytes = 0
		delete(c.entries, k)
		return false
	}
	for c.used > c.max {
		var victim vecKey
		var oldest *vecEntry
		for key, cand := range c.entries {
			if cand == e {
				continue
			}
			if oldest == nil || cand.lastUse < oldest.lastUse {
				victim, oldest = key, cand
			}
		}
		if oldest == nil {
			break
		}
		c.used -= oldest.bytes
		oldest.bytes = 0
		delete(c.entries, victim)
	}
	return true
}

func (c *vecCache) claim(k vecKey, e *vecEntry) {
	if c.entries == nil {
		c.entries = map[vecKey]*vecEntry{}
	}
	if cur := c.entries[k]; cur != e {
		if cur != nil {
			c.used -= cur.bytes
			cur.bytes = 0
		}
		c.entries[k] = e
	}
}

func (c *vecCache) remember(k vecKey, e *vecEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.claim(k, e)
}

func (c *vecCache) drop(k vecKey, e *vecEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries[k] == e {
		c.used -= e.bytes
		e.bytes = 0
		delete(c.entries, k)
	}
}

func (c *vecCache) score(ctx context.Context, tx rowsQuerier, ns, table, column string, vec []float32, threshold float64) ([]vecHit, int, bool, error) {
	if c.max <= 0 {
		return nil, 0, false, nil
	}
	db, ok := tx.(interface {
		rowsQuerier
		rowQuerier
	})
	if !ok {
		return nil, 0, false, nil
	}
	head, err := changeCounter(ctx, db)
	if err != nil {
		return nil, 0, false, err
	}
	fp, err := vectorFingerprint(ctx, db, table)
	if err != nil {
		return nil, 0, false, err
	}
	k := vecKey{ns: ns, table: table, column: column}
	e := c.entry(k)
	e.mu.Lock()
	if e.tooBig && e.fp == fp && head-e.seq < tooBigRetryChanges {
		e.mu.Unlock()
		return nil, 0, false, nil
	}
	wasTooBig := e.tooBig
	e.tooBig = false
	if wasTooBig || e.seq < 0 || e.fp != fp || e.seq > head {
		if e.seq > head {
			e.mu.Unlock()
			return nil, 0, false, nil
		}
		if err := e.rebuild(ctx, db, table, column); err != nil {
			e.mu.Unlock()
			c.drop(k, e)
			return nil, 0, false, err
		}
		e.fp, e.seq = fp, head
	} else if e.seq < head {
		ok, err := e.catchUp(ctx, db, table, column, head)
		if err != nil {
			e.mu.Unlock()
			c.drop(k, e)
			return nil, 0, false, err
		}
		if !ok {
			if err := e.rebuild(ctx, db, table, column); err != nil {
				e.mu.Unlock()
				c.drop(k, e)
				return nil, 0, false, err
			}
		}
		e.seq = head
	}
	kept := c.account(k, e, e.size())
	if !kept {
		e.tooBig, e.fp, e.seq = true, fp, head
		e.pos, e.ids, e.vecs, e.sq = nil, nil, nil, nil
		c.remember(k, e)
		e.mu.Unlock()
		return nil, 0, false, nil
	}
	e.mu.Unlock()

	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.seq != head || e.fp != fp {
		return nil, 0, false, nil
	}
	var qa float64
	for _, x := range vec {
		qa += float64(x) * float64(x)
	}
	workers := 1
	if n := len(e.vecs); n >= parallelScoreRows {
		workers = min(runtime.GOMAXPROCS(0), n/(parallelScoreRows/4))
	}
	type part struct {
		hits    []vecHit
		skipped int
	}
	parts := make([]part, workers)
	chunk := (len(e.vecs) + workers - 1) / workers
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		lo, hi := w*chunk, min((w+1)*chunk, len(e.vecs))
		wg.Add(1)
		go func(w, lo, hi int) {
			defer wg.Done()
			p := &parts[w]
			for i := lo; i < hi; i++ {
				stored := e.vecs[i]
				if stored == nil || len(stored) != len(vec) {
					p.skipped++
					continue
				}
				if score := cosineKnown(vec, stored, qa, e.sq[i]); score >= threshold {
					p.hits = append(p.hits, vecHit{id: e.ids[i], score: score})
				}
			}
		}(w, lo, hi)
	}
	wg.Wait()
	var hits []vecHit
	skipped := 0
	for _, p := range parts {
		hits = append(hits, p.hits...)
		skipped += p.skipped
	}
	return hits, skipped, true, nil
}

func vectorFingerprint(ctx context.Context, q rowQuerier, table string) (vecFingerprint, error) {
	var fp vecFingerprint
	gen, err := readNSGen(ctx, q)
	if err != nil {
		return fp, err
	}
	fp.nsGen = gen
	if fp.dropGen, err = tableGen(ctx, q, table); err != nil {
		return fp, err
	}
	if err := q.QueryRowContext(ctx, `SELECT version FROM _dolmen_tables WHERE name = ?`, table).Scan(&fp.version); err != nil {
		return fp, err
	}
	return fp, nil
}

func (e *vecEntry) size() int64 {
	var n int64
	for _, v := range e.vecs {
		n += int64(len(v)) * 4
	}
	return n + int64(len(e.ids))*vecRowOverhead
}

func (e *vecEntry) rebuild(ctx context.Context, db rowsQuerier, table, column string) error {
	e.pos = map[int64]int{}
	e.ids = e.ids[:0]
	e.vecs = e.vecs[:0]
	e.sq = e.sq[:0]
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`SELECT id, %s FROM %s WHERE %s IS NOT NULL ORDER BY id`, q(column), q(table), q(column)))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var raw any
		if err := rows.Scan(&id, &raw); err != nil {
			return err
		}
		e.put(id, raw)
	}
	return rows.Err()
}

func (e *vecEntry) put(id int64, raw any) {
	stored, ok := decodeStoredVector(raw)
	if !ok {
		stored = nil
	}
	var sq float64
	for _, x := range stored {
		sq += float64(x) * float64(x)
	}
	if i, exists := e.pos[id]; exists {
		e.vecs[i], e.sq[i] = stored, sq
		return
	}
	e.pos[id] = len(e.ids)
	e.ids = append(e.ids, id)
	e.vecs = append(e.vecs, stored)
	e.sq = append(e.sq, sq)
}

func (e *vecEntry) remove(id int64) {
	i, ok := e.pos[id]
	if !ok {
		return
	}
	last := len(e.ids) - 1
	e.ids[i], e.vecs[i], e.sq[i] = e.ids[last], e.vecs[last], e.sq[last]
	e.pos[e.ids[i]] = i
	e.ids, e.vecs, e.sq = e.ids[:last], e.vecs[:last], e.sq[:last]
	delete(e.pos, id)
}

func (e *vecEntry) catchUp(ctx context.Context, db interface {
	rowsQuerier
	rowQuerier
}, table, column string, head int64) (bool, error) {
	var oldest sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MIN(seq) FROM _dolmen_changes`).Scan(&oldest); err != nil {
		return false, err
	}
	if !oldest.Valid || oldest.Int64 > e.seq+1 {
		return false, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT row_id FROM _dolmen_changes WHERE table_name = ? AND seq > ? AND seq <= ?`, table, e.seq, head)
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
	for start := 0; start < len(touched); start += 500 {
		end := min(start+500, len(touched))
		chunk := touched[start:end]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
			e.remove(id)
		}
		ph := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		rows, err := db.QueryContext(ctx, fmt.Sprintf(`SELECT id, %s FROM %s WHERE %s IS NOT NULL AND id IN (%s)`, q(column), q(table), q(column), ph), args...)
		if err != nil {
			return false, err
		}
		for rows.Next() {
			var id int64
			var raw any
			if err := rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return false, err
			}
			e.put(id, raw)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return false, err
		}
	}
	return true, nil
}

func changeCounter(ctx context.Context, db rowQuerier) (int64, error) {
	var n sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = '_dolmen_changes'`).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return n.Int64, err
}
