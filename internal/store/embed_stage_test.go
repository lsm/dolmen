package store

import (
	"context"

	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/schema"
)

const backfillPage = 128

type backfillRecorder struct {
	calls  int
	texts  []string
	inside func(context.Context) error
}

func (r *backfillRecorder) embedder(identity string) Embedder {
	return Embedder{Identity: identity, Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		r.calls++
		r.texts = append(r.texts, texts...)
		if r.inside != nil {
			if err := r.inside(ctx); err != nil {
				return nil, err
			}
		}
		return fakeEmbed(ctx, texts)
	}}
}

func (r *backfillRecorder) embedded() int { return len(r.texts) }

func writeBound(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 5*time.Second)
}

func openBackfillStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func seedBackfillTable(t *testing.T, st *Store, ns, table string, rows int) {
	t.Helper()
	ctx := context.Background()
	if err := st.CreateNamespace(ctx, ns, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTable(ctx, ns, table, []schema.Field{{Name: "body", Type: schema.Text}}, TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	for start := 0; start < rows; start += MaxRecordsPerInsert {
		batch := make([]map[string]any, 0, MaxRecordsPerInsert)
		for i := start; i < start+MaxRecordsPerInsert && i < rows; i++ {
			batch = append(batch, map[string]any{"body": fmt.Sprintf("row %d of the backfill fixture", i)})
		}
		if _, err := st.Insert(ctx, ns, table, batch, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
			t.Fatal(err)
		}
	}
}

func vectorizeBody() []schema.Change {
	return []schema.Change{{Op: schema.OpSetVectorize, Name: "body", Value: boolPtr(true)}}
}

func liveEmbeddings(t *testing.T, st *Store, ns, table string) map[int64][]float32 {
	t.Helper()
	ctx := context.Background()
	n, err := st.ns(ns)
	if err != nil {
		t.Fatal(err)
	}
	defer n.unpin()
	var columns int
	if err := n.ro.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info(?) WHERE name = '_embedding'`, table).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns == 0 {
		return map[int64][]float32{}
	}
	rows, err := n.ro.QueryContext(ctx, fmt.Sprintf(`SELECT id, _embedding FROM %s ORDER BY id`, q(table)))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64][]float32{}
	for rows.Next() {
		var id int64
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			t.Fatal(err)
		}
		if blob == nil {
			continue
		}
		vec, err := schema.DecodeVector(blob)
		if err != nil {
			t.Fatal(err)
		}
		out[id] = vec
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func expectVector(t *testing.T, st *Store, ns, table string, id int64, want []float32) {
	t.Helper()
	got := liveEmbeddings(t, st, ns, table)[id]
	if len(got) != len(want) {
		t.Fatalf("row %d carries a %d-dimensional vector, want %d", id, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d carries the vector of some other text: component %d is %v, want %v", id, i, got[i], want[i])
		}
	}
}

func expectVectorOf(t *testing.T, st *Store, ns, table string, id int64, text string) {
	t.Helper()
	want, err := fakeEmbed(context.Background(), []string{text})
	if err != nil {
		t.Fatal(err)
	}
	expectVector(t, st, ns, table, id, want[0])
}

func migratedState(t *testing.T, st *Store, ns, table string) (int, string, int) {
	t.Helper()
	ctx := context.Background()
	sc, _, err := st.DescribeTable(ctx, ns, table, nil, Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	log, err := st.ListMigrations(ctx, ns, table, Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	return sc.Version, sc.EmbedSpace, len(log)
}

func TestMigrateEmbedsOutsideTheWriteTransaction(t *testing.T) {
	st := openBackfillStore(t)
	ctx := context.Background()
	if err := st.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"notes", "other"} {
		if _, err := st.CreateTable(ctx, "app", table, []schema.Field{{Name: "body", Type: schema.Text}}, TableOpts{}, [16]byte{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Insert(ctx, "app", "notes", []map[string]any{{"body": "one"}, {"body": "two"}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	rec := &backfillRecorder{}
	rec.inside = func(ctx context.Context) error {
		inner, cancel := writeBound(ctx)
		defer cancel()
		_, err := st.Insert(inner, "app", "other", []map[string]any{{"body": "written during backfill"}}, WriteOpts{}, Embedder{}, nil, Incarnation{})
		return err
	}
	if _, err := st.Migrate(ctx, "app", "notes", vectorizeBody(), rec.embedder("fake-space"), Incarnation{Version: 1}); err != nil {
		t.Fatalf("the backfill held the namespace writer, so a write to another table in the namespace could not land: %v", err)
	}
	if rec.calls == 0 {
		t.Fatal("the embedding provider was never called")
	}
	rows, err := st.GetRows(ctx, "app", "other", []int64{1}, nil, Incarnation{})
	if err != nil || len(rows.Rows) != 1 {
		t.Fatalf("the write made during the backfill was lost: %+v %v", rows, err)
	}
}

func TestReissuingAMigrateEmbedsOnlyWhatItHasNotStaged(t *testing.T) {
	const rows = 300
	st := openBackfillStore(t)
	seedBackfillTable(t, st, "resume", "docs", rows)
	ctx := context.Background()

	first := &backfillRecorder{}
	first.inside = func(context.Context) error {
		if first.calls >= 2 {
			return errors.New("provider is down")
		}
		return nil
	}
	if _, err := st.Migrate(ctx, "resume", "docs", vectorizeBody(), first.embedder("fake-space"), Incarnation{Version: 1}); err == nil {
		t.Fatal("a provider failure mid-backfill must fail the migration")
	}
	if first.embedded() != 2*backfillPage {
		t.Fatalf("the first attempt embedded %d rows, want two full pages of %d", first.embedded(), backfillPage)
	}
	if stage := embedStageCount(t, st, "resume", "docs"); stage != backfillPage {
		t.Fatalf("the first attempt staged %d rows, want the one page it finished before the provider failed", stage)
	}
	if live := liveEmbeddings(t, st, "resume", "docs"); len(live) != 0 {
		t.Fatalf("a failed backfill left %d live vectors, want none: nothing is activated until every row is covered", len(live))
	}
	if version, _, log := migratedState(t, st, "resume", "docs"); version != 1 || log != 0 {
		t.Fatalf("a failed backfill left the table at version %d with %d migration records, want 1 and 0", version, log)
	}

	second := &backfillRecorder{}
	after, err := st.Migrate(ctx, "resume", "docs", vectorizeBody(), second.embedder("fake-space"), Incarnation{Version: 1})
	if err != nil {
		t.Fatalf("re-issuing the migration must continue from the staged rows: %v", err)
	}
	if second.embedded() != rows-backfillPage {
		t.Fatalf("the re-issued migration embedded %d rows, want the %d the first attempt did not stage", second.embedded(), rows-backfillPage)
	}
	if after.Version != 2 || after.EmbedSpace != "fake-space" || after.EmbedDim != 8 {
		t.Fatalf("the re-issued migration ended at version %d space %q dim %d, want 2, fake-space, 8", after.Version, after.EmbedSpace, after.EmbedDim)
	}
	if _, _, log := migratedState(t, st, "resume", "docs"); log != 1 {
		t.Fatalf("the table carries %d migration records, want exactly 1", log)
	}
	for id := int64(1); id <= rows; id++ {
		expectVectorOf(t, st, "resume", "docs", id, fmt.Sprintf("row %d of the backfill fixture", id-1))
	}
	if stage := embedStageCount(t, st, "resume", "docs"); stage != 0 {
		t.Fatalf("activation left %d staged rows behind, want none", stage)
	}
}

func TestAStagedVectorIsOnlyUsedWhileTheTextStillHashesToIt(t *testing.T) {
	const rows = 200
	st := openBackfillStore(t)
	seedBackfillTable(t, st, "digest", "docs", rows)
	ctx := context.Background()

	first := &backfillRecorder{}
	first.inside = func(context.Context) error {
		if first.calls >= 2 {
			return errors.New("provider is down")
		}
		return nil
	}
	if _, err := st.Migrate(ctx, "digest", "docs", vectorizeBody(), first.embedder("fake-space"), Incarnation{Version: 1}); err == nil {
		t.Fatal("a provider failure mid-backfill must fail the migration")
	}
	if _, err := st.Update(ctx, "digest", "docs", "id = 5", nil, map[string]any{"body": "row 5 was rewritten"}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	second := &backfillRecorder{}
	if _, err := st.Migrate(ctx, "digest", "docs", vectorizeBody(), second.embedder("fake-space"), Incarnation{Version: 1}); err != nil {
		t.Fatalf("re-issuing the migration must embed the rewritten row: %v", err)
	}
	if second.embedded() != rows-backfillPage+1 {
		t.Fatalf("the re-issued migration embedded %d rows, want the %d it had not staged plus the rewritten one", second.embedded(), rows-backfillPage)
	}
	expectVectorOf(t, st, "digest", "docs", 5, "row 5 was rewritten")
	for id := int64(6); id <= rows; id++ {
		expectVectorOf(t, st, "digest", "docs", id, fmt.Sprintf("row %d of the backfill fixture", id-1))
	}
}

func TestAStagedVectorIsNotUsedAfterTheProviderChanges(t *testing.T) {
	const rows = 200
	st := openBackfillStore(t)
	seedBackfillTable(t, st, "ident", "docs", rows)
	ctx := context.Background()

	first := &backfillRecorder{}
	first.inside = func(context.Context) error {
		if first.calls >= 2 {
			return errors.New("provider is down")
		}
		return nil
	}
	if _, err := st.Migrate(ctx, "ident", "docs", vectorizeBody(), first.embedder("fake-space"), Incarnation{Version: 1}); err == nil {
		t.Fatal("a provider failure mid-backfill must fail the migration")
	}
	second := &backfillRecorder{}
	other := &countingSpace{space: "other-space", rec: second}
	after, err := st.Migrate(ctx, "ident", "docs", vectorizeBody(), other.embedder(), Incarnation{Version: 1})
	if err != nil {
		t.Fatalf("re-issuing the migration under a new provider must succeed: %v", err)
	}
	if second.embedded() != rows {
		t.Fatalf("a stage made by %q was reused under %q: only %d of %d rows were embedded", "fake-space", "other-space", second.embedded(), rows)
	}
	if after.EmbedSpace != "other-space" {
		t.Fatalf("the migration ended in embedding space %q, want other-space", after.EmbedSpace)
	}
	otherVecs, err := fakeEmbed(ctx, []string{"row 0 of the backfill fixture"})
	if err != nil {
		t.Fatal(err)
	}
	expectVector(t, st, "ident", "docs", 1, []float32{otherVecs[0][0] + 100, otherVecs[0][1], otherVecs[0][2], otherVecs[0][3], otherVecs[0][4], otherVecs[0][5], otherVecs[0][6], otherVecs[0][7]})
}

type countingSpace struct {
	space string
	rec   *backfillRecorder
}

func (c *countingSpace) embedder() Embedder {
	return Embedder{Identity: c.space, Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		c.rec.calls++
		c.rec.texts = append(c.rec.texts, texts...)
		vecs, err := fakeEmbed(ctx, texts)
		if err != nil {
			return nil, err
		}
		out := make([][]float32, len(vecs))
		for i := range vecs {
			out[i] = append([]float32(nil), vecs[i]...)
			out[i][0] += 100
		}
		return out, nil
	}}
}

func TestAStagedVectorIsNotUsedAfterTheTableIsRecreated(t *testing.T) {
	const rows = 200
	st := openBackfillStore(t)
	seedBackfillTable(t, st, "recreate", "docs", rows)
	ctx := context.Background()

	first := &backfillRecorder{}
	first.inside = func(context.Context) error {
		if first.calls >= 2 {
			return errors.New("provider is down")
		}
		return nil
	}
	if _, err := st.Migrate(ctx, "recreate", "docs", vectorizeBody(), first.embedder("fake-space"), Incarnation{Version: 1}); err == nil {
		t.Fatal("a provider failure mid-backfill must fail the migration")
	}
	if stage := embedStageCount(t, st, "recreate", "docs"); stage == 0 {
		t.Fatal("the failed migration staged nothing, so this proves nothing about recreating the table")
	}
	if err := st.DropTable(ctx, "recreate", "docs", Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if stage := embedStageCount(t, st, "recreate", "docs"); stage != 0 {
		t.Fatalf("drop_table left %d staged rows behind, want none", stage)
	}
	if _, err := st.CreateTable(ctx, "recreate", "docs", []schema.Field{{Name: "body", Type: schema.Text}}, TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Insert(ctx, "recreate", "docs", []map[string]any{{"body": "a brand new row"}}, WriteOpts{}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	second := &backfillRecorder{}
	after, err := st.Migrate(ctx, "recreate", "docs", vectorizeBody(), second.embedder("fake-space"), Incarnation{Version: 1})
	if err != nil {
		t.Fatalf("migrating the recreated table must succeed: %v", err)
	}
	if after.Version != 2 {
		t.Fatalf("the recreated table ended at version %d, want 2", after.Version)
	}
	if second.embedded() != 1 {
		t.Fatalf("the recreated table embedded %d rows, want only its own 1: a stage from the dropped table must not carry over", second.embedded())
	}
	expectVectorOf(t, st, "recreate", "docs", 1, "a brand new row")
}

func TestWritesDuringTheBackfillEndWithTheVectorOfTheirFinalText(t *testing.T) {
	for _, c := range []struct {
		name  string
		write func(ctx context.Context, st *Store) error
		id    int64
		text  string
	}{
		{
			name: "inserted ahead of the cursor",
			write: func(ctx context.Context, st *Store) error {
				inner, cancel := writeBound(ctx)
				defer cancel()
				_, err := st.Insert(inner, "wr", "docs", []map[string]any{{"body": "inserted mid-backfill"}}, WriteOpts{}, Embedder{}, nil, Incarnation{})
				return err
			},
			id:   301,
			text: "inserted mid-backfill",
		},
		{
			name: "text updated behind the cursor",
			write: func(ctx context.Context, st *Store) error {
				inner, cancel := writeBound(ctx)
				defer cancel()
				_, err := st.Update(inner, "wr", "docs", "id = 5", nil, map[string]any{"body": "row 5 rewritten behind the cursor"}, Embedder{}, nil, Incarnation{})
				return err
			},
			id:   5,
			text: "row 5 rewritten behind the cursor",
		},
		{
			name: "text updated ahead of the cursor",
			write: func(ctx context.Context, st *Store) error {
				inner, cancel := writeBound(ctx)
				defer cancel()
				_, err := st.Update(inner, "wr", "docs", "id = 260", nil, map[string]any{"body": "row 259 rewritten ahead of the cursor"}, Embedder{}, nil, Incarnation{})
				return err
			},
			id:   260,
			text: "row 259 rewritten ahead of the cursor",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			const rows = 300
			st := openBackfillStore(t)
			seedBackfillTable(t, st, "wr", "docs", rows)
			rec := &backfillRecorder{}
			rec.inside = func(ctx context.Context) error {
				if rec.calls == 2 {
					return c.write(ctx, st)
				}
				return nil
			}
			if _, err := st.Migrate(context.Background(), "wr", "docs", vectorizeBody(), rec.embedder("fake-space"), Incarnation{Version: 1}); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			expectVectorOf(t, st, "wr", "docs", c.id, c.text)
		})
	}

	t.Run("a bulk write behind the cursor", func(t *testing.T) {
		const rows = 300
		st := openBackfillStore(t)
		seedBackfillTable(t, st, "wr", "docs", rows)
		rec := &backfillRecorder{}
		rec.inside = func(ctx context.Context) error {
			if rec.calls != 2 {
				return nil
			}
			inner, cancel := writeBound(ctx)
			defer cancel()
			_, err := st.Update(inner, "wr", "docs", "id <= 600", nil, map[string]any{"body": "rewritten in bulk"}, Embedder{}, nil, Incarnation{})
			return err
		}
		if _, err := st.Migrate(context.Background(), "wr", "docs", vectorizeBody(), rec.embedder("fake-space"), Incarnation{Version: 1}); err != nil {
			t.Fatalf("a bulk write during the backfill must not stop the migration: %v", err)
		}
		for id := int64(1); id <= rows; id++ {
			expectVectorOf(t, st, "wr", "docs", id, "rewritten in bulk")
		}
	})

	t.Run("deleted", func(t *testing.T) {
		const rows = 300
		st := openBackfillStore(t)
		seedBackfillTable(t, st, "wr", "docs", rows)
		rec := &backfillRecorder{}
		rec.inside = func(ctx context.Context) error {
			if rec.calls == 2 {
				inner, cancel := writeBound(ctx)
				defer cancel()
				_, err := st.Delete(inner, "wr", "docs", "id = 5", nil, DeleteOpts{Confirm: true}, nil, Incarnation{})
				return err
			}
			return nil
		}
		if _, err := st.Migrate(context.Background(), "wr", "docs", vectorizeBody(), rec.embedder("fake-space"), Incarnation{Version: 1}); err != nil {
			t.Fatalf("a row deleted during the backfill must not stop the migration: %v", err)
		}
		if live := liveEmbeddings(t, st, "wr", "docs"); len(live) != rows-1 {
			t.Fatalf("the table carries %d vectors after one row was deleted mid-backfill, want %d", len(live), rows-1)
		}
		gone, err := st.GetRows(context.Background(), "wr", "docs", []int64{5}, nil, Incarnation{})
		if err != nil {
			t.Fatal(err)
		}
		if len(gone.Rows) != 0 {
			t.Fatalf("the row deleted during the backfill came back: %+v", gone.Rows)
		}
	})

	t.Run("emptied behind the cursor", func(t *testing.T) {
		const rows = 300
		st := openBackfillStore(t)
		seedBackfillTable(t, st, "wr", "docs", rows)
		rec := &backfillRecorder{}
		rec.inside = func(ctx context.Context) error {
			if rec.calls == 2 {
				inner, cancel := writeBound(ctx)
				defer cancel()
				if _, err := st.Update(inner, "wr", "docs", "id = 5", nil, map[string]any{"body": ""}, Embedder{}, nil, Incarnation{}); err != nil {
					return err
				}
			}
			return nil
		}
		if _, err := st.Migrate(context.Background(), "wr", "docs", vectorizeBody(), rec.embedder("fake-space"), Incarnation{Version: 1}); err != nil {
			t.Fatalf("emptying a row during the backfill must not stop the migration: %v", err)
		}
		if live, ok := liveEmbeddings(t, st, "wr", "docs")[5]; ok && len(live) > 0 {
			t.Fatalf("row 5 was emptied after its page was staged but kept the vector of its old text: a row with no text must carry no vector")
		}
	})
}

func TestAStageIsNotStampedWhenTheProviderChangesDimension(t *testing.T) {
	const rows = 200
	st := openBackfillStore(t)
	seedBackfillTable(t, st, "dims", "docs", rows)
	ctx := context.Background()

	first := &backfillRecorder{}
	first.inside = func(context.Context) error {
		if first.calls >= 2 {
			return errors.New("provider is down")
		}
		return nil
	}
	if _, err := st.Migrate(ctx, "dims", "docs", vectorizeBody(), first.embedder("fake-space"), Incarnation{Version: 1}); err == nil {
		t.Fatal("a provider failure mid-backfill must fail the migration")
	}
	if stage := embedStageCount(t, st, "dims", "docs"); stage != backfillPage {
		t.Fatalf("the first attempt staged %d rows, want the one page it finished", stage)
	}
	embedded := 0
	narrow := Embedder{Identity: "fake-space", Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		embedded += len(texts)
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 2, 3, 4}
		}
		return out, nil
	}}
	_, err := st.Migrate(ctx, "dims", "docs", vectorizeBody(), narrow, Incarnation{Version: 1})
	if err == nil {
		t.Fatal("a provider that reports one identity and two dimensions must not activate")
	}
	if !strings.Contains(err.Error(), "dimensional") {
		t.Fatalf("the refusal must name the dimension mismatch, got %v", err)
	}
	if embedded == 0 {
		t.Fatal("the second attempt embedded nothing, so the mismatch was never reached")
	}
	if live := liveEmbeddings(t, st, "dims", "docs"); len(live) != 0 {
		t.Fatalf("%d rows were stamped with a vector of the wrong length", len(live))
	}
	if version, _, log := migratedState(t, st, "dims", "docs"); version != 1 || log != 0 {
		t.Fatalf("a refused stamp left the table at version %d with %d migration records, want 1 and 0", version, log)
	}
}

func TestAMigrationThatKeepsLosingToWritersReturnsAConflict(t *testing.T) {
	st := openBackfillStore(t)
	seedBackfillTable(t, st, "busy", "docs", 200)
	ctx := context.Background()
	rec := &backfillRecorder{}
	rec.inside = func(ctx context.Context) error {
		inner, cancel := writeBound(ctx)
		defer cancel()
		_, err := st.Update(inner, "busy", "docs", "id = 1", nil, map[string]any{"body": fmt.Sprintf("row 1 rewritten at call %d", rec.calls+1)}, Embedder{}, nil, Incarnation{})
		return err
	}
	_, err := st.Migrate(ctx, "busy", "docs", vectorizeBody(), rec.embedder("fake-space"), Incarnation{Version: 1})
	if err == nil {
		t.Fatal("a migration whose rows keep changing must not activate")
	}
	if derr.Code(SpanErrorType(err)) != derr.Conflict {
		t.Fatalf("a migration that keeps losing to concurrent writes returned %q, want a conflict: re-issuing the same migrate continues from the staged rows", SpanErrorType(err))
	}
	if !errors.Is(err, derr.ErrConflict) {
		t.Fatalf("the error must be a typed conflict, got %v", err)
	}
	if rec.calls < 2 {
		t.Fatalf("the provider was called %d times, so the migration never even reached activation", rec.calls)
	}
	if version, _, log := migratedState(t, st, "busy", "docs"); version != 1 || log != 0 {
		t.Fatalf("a migration that gave up left the table at version %d with %d migration records, want 1 and 0", version, log)
	}
	if stage := embedStageCount(t, st, "busy", "docs"); stage == 0 {
		t.Fatal("a migration that gave up threw its staged work away: re-issuing it must not start from the first row")
	}
}

func TestAFormat3ReaderSeesTheTableAsItWasBeforeActivation(t *testing.T) {
	const rows = 300
	st := openBackfillStore(t)
	seedBackfillTable(t, st, "old", "docs", rows)
	ctx := context.Background()
	first := &backfillRecorder{}
	first.inside = func(context.Context) error {
		if first.calls >= 2 {
			return errors.New("provider is down")
		}
		return nil
	}
	if _, err := st.Migrate(ctx, "old", "docs", vectorizeBody(), first.embedder("fake-space"), Incarnation{Version: 1}); err == nil {
		t.Fatal("a provider failure mid-backfill must fail the migration")
	}
	if embedStageCount(t, st, "old", "docs") == 0 {
		t.Fatal("nothing was staged, so this proves nothing about what an unaware reader sees")
	}
	n, err := st.ns("old")
	if err != nil {
		t.Fatal(err)
	}
	defer n.unpin()
	sc, _, err := st.DescribeTable(ctx, "old", "docs", nil, Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	if sc.Version != 1 || sc.VectorizeField() != nil || sc.EmbedSpace != "" {
		t.Fatalf("a reader must see the pre-migration table: version %d, vectorize %v, space %q", sc.Version, sc.VectorizeField() != nil, sc.EmbedSpace)
	}
	if live := liveEmbeddings(t, st, "old", "docs"); len(live) != 0 {
		t.Fatalf("%d rows carry a live vector before activation, want none: that is what lets a reader that knows nothing about the stage behave correctly", len(live))
	}
	format, minReader, err := readCatalogStamp(t, st, "old")
	if err != nil {
		t.Fatal(err)
	}
	if format != CatalogFormat {
		t.Fatalf("the namespace reports catalog format %d, want %d", format, CatalogFormat)
	}
	if minReader != 3 || CatalogMinReader != 3 {
		t.Fatalf("the namespace needs reader %d and this binary declares %d, want both to stay at 3 so a reader that predates staging still opens it", minReader, CatalogMinReader)
	}
	if err := refuseNewerCatalog(ctx, n.rw, "old"); err != nil {
		t.Fatalf("the gate a reader that predates staging runs must let this file through: %v", err)
	}
}

func readCatalogStamp(t *testing.T, st *Store, ns string) (format int, minReader int, _ error) {
	t.Helper()
	n, err := st.ns(ns)
	if err != nil {
		t.Fatal(err)
	}
	defer n.unpin()
	format, minReader, err = readCatalogVersion(context.Background(), n.ro)
	return format, minReader, err
}

func embedStageCount(t *testing.T, st *Store, ns, table string) int64 {
	t.Helper()
	n, err := st.ns(ns)
	if err != nil {
		t.Fatal(err)
	}
	defer n.unpin()
	var count int64
	if err := n.ro.QueryRowContext(context.Background(),
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = '_dolmen_embed_stage'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		return 0
	}
	if err := n.ro.QueryRowContext(context.Background(),
		`SELECT count(*) FROM _dolmen_embed_stage WHERE table_name = ?`, table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestTheStagedPlanCountExcludesRowsWhoseTextChangedOrVanished(t *testing.T) {
	const rows = 300
	st := openBackfillStore(t)
	seedBackfillTable(t, st, "stale", "docs", rows)
	rec := &backfillRecorder{}
	rec.inside = func(context.Context) error {
		if rec.calls >= 2 {
			return errors.New("provider is down")
		}
		return nil
	}
	if _, err := st.Migrate(context.Background(), "stale", "docs", vectorizeBody(), rec.embedder("test"), Incarnation{Version: 1}); err == nil {
		t.Fatal("a provider failure mid-backfill must fail the migration")
	}
	staged := embedStageCount(t, st, "stale", "docs")
	if staged < 2 {
		t.Fatalf("the failed attempt staged %d rows, so this test cannot spoil two of them", staged)
	}
	ctx := context.Background()
	if _, err := st.Update(ctx, "stale", "docs", "id = 1", nil, map[string]any{"body": "row 1 was rewritten after it was staged"}, Embedder{}, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Delete(ctx, "stale", "docs", "id = 2", nil, DeleteOpts{}, nil, Incarnation{}); err != nil {
		t.Fatal(err)
	}
	plan, err := st.PlanMigration(ctx, "stale", "docs", vectorizeBody(), (&backfillRecorder{}).embedder("test"), Incarnation{Version: 1}, nil, Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.StagedRows != staged-2 {
		t.Fatalf("the plan reports staged_rows %d, want the %d staged rows whose text still matches, not counting the rewritten row or the deleted one", plan.StagedRows, staged-2)
	}
	if plan.EmbedRows != (rows-1)-(staged-2) {
		t.Fatalf("the plan reports embed_rows %d, want %d: a staged row whose text changed is embedded again, and a deleted row is not vectorized at all", plan.EmbedRows, (rows-1)-(staged-2))
	}
	if plan.StagedRows+plan.EmbedRows != rows-1 {
		t.Fatalf("staged_rows %d plus embed_rows %d is %d, want the %d rows that still have text", plan.StagedRows, plan.EmbedRows, plan.StagedRows+plan.EmbedRows, rows-1)
	}
}

func TestAFullyStagedPlanAsksTheProviderForNothing(t *testing.T) {
	const rows = 300
	st := openBackfillStore(t)
	seedBackfillTable(t, st, "full", "docs", rows)
	ctx := context.Background()
	rec := &backfillRecorder{}
	rec.inside = func(ctx context.Context) error {
		if rec.calls != 1 {
			return nil
		}
		inner, cancel := writeBound(ctx)
		defer cancel()
		_, err := st.Migrate(inner, "full", "docs", []schema.Change{
			{Op: schema.OpAddField, Field: &schema.Field{Name: "note", Type: schema.String}},
		}, Embedder{}, Incarnation{Version: 1})
		return err
	}
	_, err := st.Migrate(ctx, "full", "docs", vectorizeBody(), rec.embedder("test"), Incarnation{Version: 1})
	if err == nil {
		t.Fatal("the migration must not land: a second migration of the same table lands during its backfill")
	}
	var versioned *VersionConflictError
	if !errors.As(err, &versioned) {
		t.Fatalf("a migration whose expected_version was overtaken by another migration of the same table returned %v, want a version conflict", err)
	}
	if versioned.ExpectedVersion != 1 || versioned.CurrentVersion != 2 {
		t.Fatalf("the conflict is about versions %d and %d, want 1 and 2", versioned.ExpectedVersion, versioned.CurrentVersion)
	}
	if version, _, log := migratedState(t, st, "full", "docs"); version != 2 || log != 1 {
		t.Fatalf("the table is at version %d with %d migration records, want 2 and 1: only the add_field landed", version, log)
	}
	staged := embedStageCount(t, st, "full", "docs")
	if staged != rows {
		t.Fatalf("%d rows are staged, want all %d: the backfill finished before the version check refused the activation", staged, rows)
	}
	plan, err := st.PlanMigration(ctx, "full", "docs", vectorizeBody(), (&backfillRecorder{}).embedder("test"), Incarnation{Version: 2}, nil, Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.StagedRows != rows {
		t.Fatalf("the plan reports staged_rows %d, want all %d", plan.StagedRows, rows)
	}
	if plan.EmbedRows != 0 {
		t.Fatalf("the plan reports embed_rows %d, want 0: every row's vector is already staged and its text still matches, so the re-issue asks the provider for nothing", plan.EmbedRows)
	}
	if plan.StagedRows+plan.EmbedRows != rows {
		t.Fatalf("staged_rows %d plus embed_rows %d is %d, want the %d rows the change will vectorize", plan.StagedRows, plan.EmbedRows, plan.StagedRows+plan.EmbedRows, rows)
	}
	again := &backfillRecorder{}
	if _, err := st.Migrate(ctx, "full", "docs", vectorizeBody(), again.embedder("test"), Incarnation{Version: 2}); err != nil {
		t.Fatalf("re-issuing the migration at the version the add_field left must land: %v", err)
	}
	if again.calls != 0 {
		t.Fatalf("the re-issued migration called the provider %d times, want none: every row was already staged", again.calls)
	}
	if live := liveEmbeddings(t, st, "full", "docs"); len(live) != rows {
		t.Fatalf("%d rows carry a vector, want %d", len(live), rows)
	}
}
