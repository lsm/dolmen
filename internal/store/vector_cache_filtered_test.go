package store

import (
	"context"
	"fmt"
	"math/rand"
	"reflect"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func openOwnPair(t *testing.T) (cached, plain *Store) {
	t.Helper()
	open := func(n int64) *Store {
		st, err := Open(t.TempDir(), WithVectorCacheBytes(n))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		mustNS(t, legacy(st), "v")
		if _, err := st.CreateTable(context.Background(), "v", "t", []schema.Field{{Name: "k", Type: schema.Number}, {Name: "tag", Type: schema.String}, {Name: "emb", Type: schema.Vector, Dim: 3}}, TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); err != nil {
			t.Fatal(err)
		}
		return st
	}
	return open(DefaultVectorCacheBytes), open(0)
}

func TestFilteredAndScopedVectorSearchesAnswerAlikeFromTheCache(t *testing.T) {
	cached, plain := openOwnPair(t)
	ctx := context.Background()
	rng := rand.New(rand.NewSource(11))
	owners := []string{"alice", "bob", "carol"}
	write := func(f func(st *Store) error) {
		t.Helper()
		for _, st := range []*Store{cached, plain} {
			if err := f(st); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := 0; i < 90; i++ {
		v := []float32{rng.Float32()*2 - 1, rng.Float32()*2 - 1, rng.Float32()*2 - 1}
		rec := map[string]any{"k": i, "tag": fmt.Sprintf("t%d", i%4), "emb": v}
		if i%9 == 0 {
			delete(rec, "emb")
		}
		owner := owners[i%3]
		write(func(st *Store) error {
			_, err := st.Insert(ctx, "v", "t", []map[string]any{rec}, WriteOpts{Owner: owner}, testEmbed, nil, Incarnation{})
			return err
		})
	}
	min := 0.1
	check := func(step string) {
		t.Helper()
		for _, scope := range []*RowScope{nil, {Owner: "alice"}, {Owner: "bob"}, {Owner: "nobody"}, {Empty: true}} {
			for _, f := range []struct {
				filter string
				args   []any
			}{{"", nil}, {"tag = ?", []any{"t1"}}, {"k < ?", []any{40}}, {"k > 1000", nil}} {
				for _, pg := range []Page{{Limit: 10}, {Offset: 3, Limit: 4}} {
					for _, ms := range []*float64{nil, &min} {
						vq := VectorQuery{Column: "emb", Vec: []float32{0.4, -0.2, 0.9}, Filter: f.filter, Args: append([]any(nil), f.args...), MinScore: ms}
						a, errA := cached.SearchVector(ctx, "v", "t", vq, false, scope, Incarnation{}, pg)
						vq.Args = append([]any(nil), f.args...)
						b, errB := plain.SearchVector(ctx, "v", "t", vq, false, scope, Incarnation{}, pg)
						if (errA == nil) != (errB == nil) {
							t.Fatalf("%s: errors differ: %v vs %v", step, errA, errB)
						}
						if errA != nil {
							continue
						}
						if !reflect.DeepEqual(scopedShape(a), scopedShape(b)) {
							t.Fatalf("%s: scope %+v filter %q page %+v: cached answered differently\ncached: %+v\nplain:  %+v", step, scope, f.filter, pg, scopedShape(a), scopedShape(b))
						}
					}
				}
			}
		}
	}
	write(func(st *Store) error {
		n, err := st.ns("v")
		if err != nil {
			return err
		}
		defer n.unpin()
		_, err = n.rw.Exec(`UPDATE t SET emb = x'0102' WHERE k = 5`)
		return err
	})
	check("after inserts, with a corrupt vector")
	write(func(st *Store) error {
		_, err := legacy(st).Update(ctx, "v", "t", "k < ?", []any{20}, map[string]any{"tag": "t1", "emb": []float32{1, 1, 1}}, testEmbed)
		return err
	})
	check("after an update")
}

func TestAFilterErrorReadsTheSameWithTheCache(t *testing.T) {
	cached, plain := openOwnPair(t)
	ctx := context.Background()
	vq := VectorQuery{Column: "emb", Vec: []float32{1, 0, 0}, Filter: "nosuchcol = 1"}
	_, errA := cached.SearchVector(ctx, "v", "t", vq, false, nil, Incarnation{}, Page{Limit: 5})
	_, errB := plain.SearchVector(ctx, "v", "t", vq, false, nil, Incarnation{}, Page{Limit: 5})
	if errA == nil || errB == nil || errA.Error() != errB.Error() {
		t.Fatalf("a bad filter must fail the same way: %v vs %v", errA, errB)
	}
}

func scopedShape(r SearchResult) searchSummary {
	out := searchSummary{Truncated: r.Truncated, Skipped: r.SkippedVectors}
	for _, row := range r.Rows {
		out.Keys = append(out.Keys, row["k"])
		out.Scores = append(out.Scores, row["_score"])
	}
	return out
}

func TestAFilteredSearchIsServedFromTheCache(t *testing.T) {
	cached, _ := openOwnPair(t)
	ctx := context.Background()
	if _, err := cached.Insert(ctx, "v", "t", []map[string]any{{"k": 1, "tag": "a", "emb": []float32{1, 0, 0}}}, WriteOpts{Owner: "alice"}, testEmbed, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	vq := VectorQuery{Column: "emb", Vec: []float32{1, 0, 0}, Filter: "tag = ?", Args: []any{"a"}}
	if _, err := cached.SearchVector(ctx, "v", "t", vq, false, &RowScope{Owner: "alice"}, Incarnation{}, Page{Limit: 5}); err != nil {
		t.Fatal(err)
	}
	if len(cached.vcache.entries) != 1 {
		t.Fatalf("a filtered, scoped search must fill the cache, entries: %d", len(cached.vcache.entries))
	}
}

func TestAScopedSearchWithoutAFilterIsServedFromTheCache(t *testing.T) {
	cached, _ := openOwnPair(t)
	ctx := context.Background()
	if _, err := cached.Insert(ctx, "v", "t", []map[string]any{{"k": 1, "emb": []float32{1, 0, 0}}}, WriteOpts{Owner: "alice"}, testEmbed, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	n, err := cached.ns("v")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.rw.Exec(`UPDATE t SET owner = 'mallory'`); err != nil {
		t.Fatal(err)
	}
	n.unpin()
	res, err := cached.SearchVector(ctx, "v", "t", VectorQuery{Column: "emb", Vec: []float32{1, 0, 0}}, false, &RowScope{Owner: "mallory"}, Incarnation{}, Page{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("the first scoped search builds the cache from the table: %d rows", len(res.Rows))
	}
	if len(cached.vcache.entries) != 1 {
		t.Fatal("a scoped search must fill the cache")
	}
}

func TestAFilterIsNotRunForTheCacheWhenTheTableDoesNotFit(t *testing.T) {
	st, err := Open(t.TempDir(), WithVectorCacheBytes(64))
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
	if _, err := l.Insert(ctx, "v", "t", []map[string]any{{"k": 1, "emb": []float32{1, 0, 0}}, {"k": 2, "emb": []float32{0, 1, 0}}}, testEmbed); err != nil {
		t.Fatal(err)
	}
	n, err := st.ns("v")
	if err != nil {
		t.Fatal(err)
	}
	defer n.unpin()
	for i := 0; i < 2; i++ {
		calls := 0
		_, _, cached, err := st.vcache.score(ctx, n.ro, "v", "t", "emb", []float32{1, 0, 0}, -1, func() ([]int64, error) {
			calls++
			return []int64{1}, nil
		}, nil, false)
		if err != nil || cached || calls != 0 {
			t.Fatalf("search %d on a table too big for the cache must not run the filter for it: cached=%v calls=%d %v", i, cached, calls, err)
		}
	}
}
