package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestPostgresInsertAtomicChanges(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	other := openTest(t, cfg)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body"}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	failures := make(chan error, 16)
	for i := range 16 {
		wg.Go(func() {
			client := s
			if i%2 == 0 {
				client = other
			}
			_, err := client.Insert(ctx, "app", "notes", []map[string]any{{"body": fmt.Sprint(i)}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{})
			if err != nil {
				failures <- err
			}
		})
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	var count, min, max, counter int64
	err := s.pool.QueryRow(ctx, "SELECT count(*),min(position),max(position) FROM "+s.relation("changes")+" WHERE namespace='app'").Scan(&count, &min, &max)
	if err != nil || count != 16 || min != 1 || max != 16 {
		t.Fatalf("commit order: %d %d %d %v", count, min, max, err)
	}
	_, err = s.pool.Exec(ctx, "CREATE FUNCTION "+ident(s.catalog, "reject_change")+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected log failure'; END $$`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, "CREATE TRIGGER reject_change BEFORE INSERT ON "+s.relation("changes")+" FOR EACH ROW EXECUTE FUNCTION "+ident(s.catalog, "reject_change")+"()"); err != nil {
		t.Fatal(err)
	}
	opts := store.WriteOpts{IdempotencyKey: "failed"}
	result, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "rollback"}}, opts, store.Embedder{}, nil, store.Incarnation{})
	if err == nil || len(result.Ids) != 0 {
		t.Fatalf("failure leaked result: %+v %v", result, err)
	}
	_, rows, err := s.DescribeTable(ctx, "app", "notes", nil, store.Incarnation{})
	if err != nil || rows != 16 {
		t.Fatalf("partial rows: %d %v", rows, err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT next_change FROM "+s.relation("namespaces")+" WHERE name='app'").Scan(&counter); err != nil || counter != 16 {
		t.Fatalf("counter consumed: %d %v", counter, err)
	}
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM "+s.relation("idempotency")).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial idempotency: %d %v", count, err)
	}
	if _, err := s.pool.Exec(ctx, "DROP TRIGGER reject_change ON "+s.relation("changes")); err != nil {
		t.Fatal(err)
	}
	result, err = s.Insert(ctx, "app", "notes", []map[string]any{{"body": "rollback"}}, opts, store.Embedder{}, nil, store.Incarnation{})
	if err != nil || result.Replayed || result.Changes.First != 17 {
		t.Fatalf("retry after rollback: %+v %v", result, err)
	}
	if err := s.DropTable(ctx, "app", "notes", store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body"}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	result, err = s.Insert(ctx, "app", "notes", []map[string]any{{"body": "new lifetime"}}, opts, store.Embedder{}, nil, store.Incarnation{})
	if err != nil || result.Replayed {
		t.Fatalf("old idempotency reused: %+v %v", result, err)
	}
}

func TestPostgresInsertEmbedsOutsideTransaction(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	fields := []schema.Field{{Name: "body", Vectorize: true}}
	if _, err := s.CreateTable(ctx, "app", "notes", fields, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	emb := store.Embedder{Identity: "test", Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		calls++
		if err := s.write(ctx, "app", [16]byte{}, func(tx pgx.Tx, n namespace) error { return nil }); err != nil {
			return nil, err
		}
		return [][]float32{{1, 0}}, nil
	}}
	records := []map[string]any{{"body": "hello"}}
	opts := store.WriteOpts{IdempotencyKey: "embedded"}
	first, err := s.Insert(ctx, "app", "notes", records, opts, emb, nil, store.Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.Insert(ctx, "app", "notes", records, opts, store.Embedder{}, nil, store.Incarnation{})
	if err != nil || !replay.Replayed || calls != 1 {
		t.Fatalf("replay called embedder: %d %+v %v", calls, replay, err)
	}
	sc, _, err := s.TableState(ctx, "app", "notes", nil)
	if err != nil || sc.EmbedDim != 2 || sc.EmbedSpace != "test" {
		t.Fatalf("embedding metadata: %+v %v", sc, err)
	}
	rows, err := s.GetRows(ctx, "app", "notes", first.Ids, nil, store.Incarnation{})
	if err != nil || len(rows.Rows) != 1 {
		t.Fatal(err)
	}
	emb.Embed = func(ctx context.Context, texts []string) ([][]float32, error) {
		if err := s.DropTable(ctx, "app", "notes", store.Incarnation{}); err != nil {
			return nil, err
		}
		if _, err := s.CreateTable(ctx, "app", "notes", fields, store.TableOpts{}, [16]byte{}); err != nil {
			return nil, err
		}
		return [][]float32{{1, 0}}, nil
	}
	if _, err := s.Insert(ctx, "app", "notes", records, store.WriteOpts{}, emb, nil, store.Incarnation{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("replacement accepted: %v", err)
	}
}

func TestPostgresNumberAffinity(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want any
	}{
		{"-0.0", int64(0)}, {"9007199254740993.0", int64(9007199254740993)},
		{"9223372036854775807.0", int64(9223372036854775807)},
		{"1e2", int64(100)}, {"1e20", float64(1e20)}, {"2.5", float64(2.5)},
	} {
		got, err := decodeNumber(schema.Field{Name: "n", Type: schema.Number}, test.raw)
		if err != nil || got != test.want {
			t.Fatalf("%s: %#v want %#v (%v)", test.raw, got, test.want, err)
		}
	}
}

func TestPostgresOwnerStampedOnEveryInsertingPath(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	fields := []schema.Field{{Name: "body", Type: schema.Text}, {Name: "natural_key", Type: schema.Text}}
	if _, err := s.CreateTable(ctx, "app", "notes", fields, store.TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	owner := store.WriteOpts{Owner: "alice"}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "direct", "natural_key": "a"}}, owner, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Upsert(ctx, "app", "notes", "body = 'absent'", nil, map[string]any{"body": "upserted", "natural_key": "b"}, owner, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertByKey(ctx, "app", "notes", []string{"natural_key"}, []map[string]any{{"body": "keyed", "natural_key": "c"}}, owner, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	scoped := &store.RowScope{Owner: "alice"}
	rows, err := s.GetRows(ctx, "app", "notes", []int64{1, 2, 3}, scoped, store.Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows.Rows) != 3 {
		t.Fatalf("every inserting path must stamp the owner, or the row is invisible to the principal who wrote it; an owner-scoped read saw %d of 3 rows", len(rows.Rows))
	}
	for _, row := range rows.Rows {
		if row[schema.OwnerColumn] != "alice" {
			t.Fatalf("row %v carries owner %v, want alice", row["body"], row[schema.OwnerColumn])
		}
	}
}

func TestPostgresInsertReportsAStaleScopeIncarnationAsConflict(t *testing.T) {
	cfg := testConfig(t)
	s := openTest(t, cfg)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	fields := []schema.Field{{Name: "body", Type: schema.Text}}
	if _, err := s.CreateTable(ctx, "app", "notes", fields, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	_, inc, err := s.TableState(ctx, "app", "notes", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DropTable(ctx, "app", "notes", inc); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", fields, store.TableOpts{}, inc.NsGen); err != nil {
		t.Fatal(err)
	}
	_, err = s.Insert(ctx, "app", "notes", []map[string]any{{"body": "x"}}, store.WriteOpts{}, store.Embedder{}, nil, inc)
	if !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("an insert whose scope was resolved against the dropped table must conflict so the caller re-resolves, the way SQLite does: %v", err)
	}
}
