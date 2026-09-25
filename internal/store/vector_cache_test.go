package store

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"sync"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

type vecPair struct {
	cached, plain legacyStore
}

func openVecPair(t *testing.T, cacheBytes int64) vecPair {
	t.Helper()
	open := func(n int64) legacyStore {
		st, err := Open(t.TempDir(), WithVectorCacheBytes(n))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		l := legacy(st)
		mustNS(t, l, "v")
		if _, err := l.CreateTable(context.Background(), "v", "t", []schema.Field{{Name: "k", Type: schema.Number}, {Name: "emb", Type: schema.Vector, Dim: 3}}); err != nil {
			t.Fatal(err)
		}
		return l
	}
	return vecPair{cached: open(cacheBytes), plain: open(0)}
}

func (p vecPair) both(t *testing.T, f func(l legacyStore) error) {
	t.Helper()
	for _, l := range []legacyStore{p.cached, p.plain} {
		if err := f(l); err != nil {
			t.Fatal(err)
		}
	}
}

func (p vecPair) same(t *testing.T, step string) {
	t.Helper()
	ctx := context.Background()
	min := 0.2
	for _, qv := range [][]float32{{1, 0, 0}, {0.3, -1, 0.5}, {-1, -1, -1}} {
		for _, c := range []struct {
			offset, limit int
			minScore      *float64
		}{{0, 10, nil}, {3, 5, nil}, {0, 200, &min}} {
			a, err := p.cached.SearchVector(ctx, "v", "t", "emb", qv, "", c.offset, c.limit, false, "", nil, c.minScore)
			if err != nil {
				t.Fatalf("%s: cached search: %v", step, err)
			}
			b, err := p.plain.SearchVector(ctx, "v", "t", "emb", qv, "", c.offset, c.limit, false, "", nil, c.minScore)
			if err != nil {
				t.Fatalf("%s: plain search: %v", step, err)
			}
			if !reflect.DeepEqual(searchShape(a), searchShape(b)) {
				t.Fatalf("%s: query %v %+v: cached search answered differently from the scan\ncached: %+v\nplain:  %+v", step, qv, c, searchShape(a), searchShape(b))
			}
		}
	}
}

func TestTheVectorCacheAnswersExactlyAsTheScanThroughEveryWrite(t *testing.T) {
	p := openVecPair(t, DefaultVectorCacheBytes)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(7))
	vec := func() []float32 { return []float32{rng.Float32()*2 - 1, rng.Float32()*2 - 1, rng.Float32()*2 - 1} }
	for i := 0; i < 40; i++ {
		v := vec()
		p.both(t, func(l legacyStore) error {
			_, err := l.Insert(ctx, "v", "t", []map[string]any{{"k": i, "emb": v}}, testEmbed)
			return err
		})
	}
	p.same(t, "after inserts")
	for step := 0; step < 60; step++ {
		k := rng.Intn(40)
		v := vec()
		var name string
		switch step % 4 {
		case 0:
			name = fmt.Sprintf("update k=%d", k)
			p.both(t, func(l legacyStore) error {
				_, err := l.Update(ctx, "v", "t", "k = ?", []any{k}, map[string]any{"emb": v}, testEmbed)
				return err
			})
		case 1:
			name = fmt.Sprintf("delete k=%d", k)
			p.both(t, func(l legacyStore) error {
				_, err := l.Delete(ctx, "v", "t", "k = ?", []any{k}, DeleteOptions{})
				return err
			})
		case 2:
			name = fmt.Sprintf("upsert k=%d", k)
			p.both(t, func(l legacyStore) error {
				_, _, _, err := l.UpsertByKey(ctx, "v", "t", []string{"k"}, []map[string]any{{"k": k, "emb": v}}, testEmbed)
				return err
			})
		case 3:
			name = fmt.Sprintf("clear k=%d", k)
			p.both(t, func(l legacyStore) error {
				_, err := l.Update(ctx, "v", "t", "k = ?", []any{k}, map[string]any{"emb": nil}, testEmbed)
				return err
			})
		}
		p.same(t, name)
	}
}

func TestTheVectorCacheCountsCorruptVectorsAsSkipped(t *testing.T) {
	p := openVecPair(t, DefaultVectorCacheBytes)
	ctx := context.Background()
	p.both(t, func(l legacyStore) error {
		_, err := l.Insert(ctx, "v", "t", []map[string]any{{"k": 1, "emb": []float32{1, 0, 0}}, {"k": 2, "emb": []float32{0, 1, 0}}, {"k": 3, "emb": []float32{0, 0, 1}}}, testEmbed)
		return err
	})
	for _, l := range []legacyStore{p.cached, p.plain} {
		n, err := l.ns("v")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := n.rw.Exec(`UPDATE t SET emb = x'0102' WHERE k = 2`); err != nil {
			t.Fatal(err)
		}
		n.unpin()
	}
	p.same(t, "a corrupt stored vector")
	res, err := p.cached.SearchVector(ctx, "v", "t", "emb", []float32{1, 0, 0}, "", 0, 10, false, "", nil, nil)
	if err != nil || res.Skipped != 1 || len(res.Rows) != 2 {
		t.Fatalf("the cached search must skip and count the corrupt vector: %d rows, %d skipped, %v", len(res.Rows), res.Skipped, err)
	}
	p.both(t, func(l legacyStore) error {
		_, err := l.Update(ctx, "v", "t", "k = ?", []any{2}, map[string]any{"emb": []float32{0, 1, 0}}, testEmbed)
		return err
	})
	p.same(t, "after the corrupt vector was rewritten")
}

func TestTheVectorCacheFollowsATableDroppedAndRecreated(t *testing.T) {
	p := openVecPair(t, DefaultVectorCacheBytes)
	ctx := context.Background()
	p.both(t, func(l legacyStore) error {
		_, err := l.Insert(ctx, "v", "t", []map[string]any{{"k": 1, "emb": []float32{1, 0, 0}}}, testEmbed)
		return err
	})
	p.same(t, "before drop")
	p.both(t, func(l legacyStore) error {
		if err := l.DropTable(ctx, "v", "t"); err != nil {
			return err
		}
		if _, err := l.CreateTable(ctx, "v", "t", []schema.Field{{Name: "k", Type: schema.Number}, {Name: "emb", Type: schema.Vector, Dim: 3}}); err != nil {
			return err
		}
		_, err := l.Insert(ctx, "v", "t", []map[string]any{{"k": 9, "emb": []float32{0, 0, 1}}}, testEmbed)
		return err
	})
	p.same(t, "after drop and recreate")
}

func TestTheVectorCacheRebuildsWhenTheChangeLogWasPruned(t *testing.T) {
	p := openVecPair(t, DefaultVectorCacheBytes)
	ctx := context.Background()
	p.both(t, func(l legacyStore) error {
		_, err := l.Insert(ctx, "v", "t", []map[string]any{{"k": 1, "emb": []float32{1, 0, 0}}}, testEmbed)
		return err
	})
	p.same(t, "warm")
	for _, l := range []legacyStore{p.cached, p.plain} {
		if _, err := l.Insert(ctx, "v", "t", []map[string]any{{"k": 2, "emb": []float32{0, 1, 0}}}, testEmbed); err != nil {
			t.Fatal(err)
		}
		n, err := l.ns("v")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := n.rw.Exec(`DELETE FROM _dolmen_changes`); err != nil {
			t.Fatal(err)
		}
		n.unpin()
	}
	p.same(t, "after the changes since the cache were pruned")
}

func TestAVectorCacheTooSmallForATableStillAnswers(t *testing.T) {
	p := openVecPair(t, 64)
	ctx := context.Background()
	p.both(t, func(l legacyStore) error {
		_, err := l.Insert(ctx, "v", "t", []map[string]any{{"k": 1, "emb": []float32{1, 0, 0}}, {"k": 2, "emb": []float32{0, 1, 0}}, {"k": 3, "emb": []float32{0, 0, 1}}}, testEmbed)
		return err
	})
	p.same(t, "a table larger than the cache")
	c := &p.cached.vcache
	if c.used != 0 {
		t.Fatalf("a table larger than the cache must hold no memory, %d bytes accounted", c.used)
	}
	for _, e := range c.entries {
		if !e.tooBig || e.vecs != nil {
			t.Fatalf("a table larger than the cache must be remembered as too big, not rebuilt on every search: %+v", e)
		}
	}
}

func TestTheVectorCacheUnderConcurrentWritesAndSearches(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	l := legacy(st)
	mustNS(t, l, "v")
	ctx := context.Background()
	if _, err := l.CreateTable(ctx, "v", "t", []schema.Field{{Name: "k", Type: schema.Number}, {Name: "emb", Type: schema.Vector, Dim: 3}}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				if _, err := l.Insert(ctx, "v", "t", []map[string]any{{"k": w*100 + i, "emb": []float32{float32(w), float32(i), 1}}}, testEmbed); err != nil {
					errs <- err
					return
				}
				if _, err := l.SearchVector(ctx, "v", "t", "emb", []float32{1, 1, 1}, "", 0, 5, false, "", nil, nil); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	res, err := l.SearchVector(ctx, "v", "t", "emb", []float32{1, 1, 1}, "", 0, 200, false, "", nil, nil)
	if err != nil || len(res.Rows) != 200 {
		t.Fatalf("after 200 concurrent inserts the cache must see all 200 rows: %d %v", len(res.Rows), err)
	}
}

type searchSummary struct {
	Keys      []any
	Scores    []any
	Truncated bool
	Skipped   int
}

func searchShape(r VectorSearchResult) searchSummary {
	out := searchSummary{Truncated: r.Truncated, Skipped: r.Skipped}
	for _, row := range r.Rows {
		out.Keys = append(out.Keys, row["k"])
		out.Scores = append(out.Scores, row["_score"])
	}
	return out
}

func TestATooBigTableThatLaterFitsIsRebuiltWhole(t *testing.T) {
	p := openVecPair(t, 64)
	ctx := context.Background()
	p.both(t, func(l legacyStore) error {
		_, err := l.Insert(ctx, "v", "t", []map[string]any{{"k": 1, "emb": []float32{1, 0, 0}}, {"k": 2, "emb": []float32{0, 1, 0}}, {"k": 3, "emb": []float32{0, 0, 1}}}, testEmbed)
		return err
	})
	p.same(t, "too big")
	batch := make([]map[string]any, tooBigRetryChanges)
	for i := range batch {
		batch[i] = map[string]any{"k": 100 + i, "emb": []float32{float32(i%7) - 3, 1, float32(i % 5)}}
	}
	p.both(t, func(l legacyStore) error {
		_, err := l.Insert(ctx, "v", "t", batch, testEmbed)
		return err
	})
	c := &p.cached.vcache
	c.mu.Lock()
	c.max = DefaultVectorCacheBytes
	c.mu.Unlock()
	p.same(t, "a too-big table retried after it fits")
	var sum int64
	for _, e := range c.entries {
		sum += e.bytes
	}
	if c.used != sum {
		t.Fatalf("cache accounting drifted: used %d, entries hold %d", c.used, sum)
	}
}

func TestAnEntryEvictedMidBuildIsAccountedOnceWhenItReturns(t *testing.T) {
	c := &vecCache{max: 100}
	ka, kb := vecKey{table: "a"}, vecKey{table: "b"}
	a := c.entry(ka)
	b := c.entry(kb)
	if !c.account(kb, b, 60) {
		t.Fatal("b fits")
	}
	if !c.account(ka, a, 60) {
		t.Fatal("a fits after evicting b")
	}
	if !c.account(kb, b, 30) {
		t.Fatal("b fits again")
	}
	if c.entries[kb] != b {
		t.Fatal("an entry evicted while it was being built must be cached again when it is accounted")
	}
	var sum int64
	for _, e := range c.entries {
		sum += e.bytes
	}
	if c.used != sum || c.used > c.max {
		t.Fatalf("accounting drifted: used %d, entries hold %d, max %d", c.used, sum, c.max)
	}
	if c.account(kb, b, 500) {
		t.Fatal("b cannot fit")
	}
	sum = 0
	for _, e := range c.entries {
		sum += e.bytes
	}
	if c.used != sum || c.used < 0 {
		t.Fatalf("accounting drifted after a rejection: used %d, entries hold %d", c.used, sum)
	}
}
