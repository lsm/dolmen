package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

type pgStageRecorder struct {
	calls  int
	texts  []string
	inside func(context.Context) error
}

func (r *pgStageRecorder) embedder(identity string) store.Embedder {
	return store.Embedder{Identity: identity, Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		r.calls++
		r.texts = append(r.texts, texts...)
		if r.inside != nil {
			if err := r.inside(ctx); err != nil {
				return nil, err
			}
		}
		out := make([][]float32, len(texts))
		for i, text := range texts {
			v := make([]float32, 8)
			for _, b := range []byte(text) {
				v[b%8]++
			}
			out[i] = v
		}
		return out, nil
	}}
}

func (r *pgStageRecorder) embedded() int { return len(r.texts) }

func seedPGStageTable(t *testing.T, s *Store, ns, table string, rows int) {
	t.Helper()
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, ns, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, ns, table, []schema.Field{{Name: "body", Type: schema.Text}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	for start := 0; start < rows; start += store.MaxRecordsPerInsert {
		batch := make([]map[string]any, 0, store.MaxRecordsPerInsert)
		for i := start; i < start+store.MaxRecordsPerInsert && i < rows; i++ {
			batch = append(batch, map[string]any{"body": fmt.Sprintf("row %d of the backfill fixture", i)})
		}
		if _, err := s.Insert(ctx, ns, table, batch, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
			t.Fatal(err)
		}
	}
}

func vectorizeBody() []schema.Change {
	on := true
	return []schema.Change{{Op: schema.OpSetVectorize, Name: "body", Value: &on}}
}

func pgStageCount(t *testing.T, s *Store, ns, table string) int64 {
	t.Helper()
	var n int64
	if err := s.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+s.relation("embed_stage")+
		" WHERE namespace = $1 AND table_name = $2", ns, table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func pgLiveEmbeddings(t *testing.T, s *Store, ns, table string) int64 {
	t.Helper()
	var n int64
	if err := s.read(t.Context(), ns, func(tx pgx.Tx, nsp namespace) error {
		state, err := s.loadTable(t.Context(), tx, nsp, table)
		if err != nil {
			return err
		}
		physical := ident(nsp.physical, state.physical)
		var hasColumn bool
		if err := tx.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_attribute
			WHERE attrelid = $1::regclass AND attname = '_embedding')`, physical).Scan(&hasColumn); err != nil {
			return err
		}
		if !hasColumn {
			return nil
		}
		return tx.QueryRow(t.Context(), `SELECT count(*) FROM `+physical+` WHERE "_embedding" IS NOT NULL`).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPostgresReissuingAMigrateEmbedsOnlyWhatItHasNotStaged(t *testing.T) {
	const rows = 300
	s := openTest(t, testConfig(t))
	seedPGStageTable(t, s, "resume", "docs", rows)
	ctx := t.Context()

	first := &pgStageRecorder{}
	first.inside = func(context.Context) error {
		if first.calls >= 2 {
			return errors.New("provider is down")
		}
		return nil
	}
	if _, err := s.Migrate(ctx, "resume", "docs", vectorizeBody(), first.embedder("test"), store.Incarnation{Version: 1}); err == nil {
		t.Fatal("a provider failure mid-backfill must fail the migration")
	}
	if first.embedded() != 2*embedBackfillPage {
		t.Fatalf("the first attempt embedded %d rows, want two full pages of %d", first.embedded(), embedBackfillPage)
	}
	if stage := pgStageCount(t, s, "resume", "docs"); stage != embedBackfillPage {
		t.Fatalf("the first attempt staged %d rows, want the one page it finished before the provider failed", stage)
	}
	if live := pgLiveEmbeddings(t, s, "resume", "docs"); live != 0 {
		t.Fatalf("a failed backfill left %d live vectors, want none", live)
	}

	second := &pgStageRecorder{}
	after, err := s.Migrate(ctx, "resume", "docs", vectorizeBody(), second.embedder("test"), store.Incarnation{Version: 1})
	if err != nil {
		t.Fatalf("re-issuing the migration must continue from the staged rows: %v", err)
	}
	if second.embedded() != rows-embedBackfillPage {
		t.Fatalf("the re-issued migration embedded %d rows, want the %d the first attempt did not stage", second.embedded(), rows-embedBackfillPage)
	}
	if after.Version != 2 || after.EmbedSpace != "test" || after.EmbedDim != 8 {
		t.Fatalf("the re-issued migration ended at version %d space %q dim %d, want 2, test, 8", after.Version, after.EmbedSpace, after.EmbedDim)
	}
	if live := pgLiveEmbeddings(t, s, "resume", "docs"); live != rows {
		t.Fatalf("%d rows carry a vector after activation, want %d", live, rows)
	}
	if stage := pgStageCount(t, s, "resume", "docs"); stage != 0 {
		t.Fatalf("activation left %d staged rows behind, want none", stage)
	}
}

func TestPostgresTheSQLDigestIsTheDigest(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "digest", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	texts := []string{
		"plain ascii",
		"café",
		"naïve résumé",
		"an emoji 🌈 and a flag 🏳️‍🌈",
		"a non-breaking space",
		"mixed é and 🌈 and   together",
		"",
		"a much longer sentence that spans several words and repeats words words words",
	}
	for i, text := range texts {
		var got []byte
		if err := s.pool.QueryRow(ctx, "SELECT sha256(convert_to($1, 'UTF8'))", text).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if want := sha256.Sum256([]byte(text)); !bytesEqual(got, want[:]) {
			t.Fatalf("text %d %q: SQL digest %x differs from textDigest %x", i, text, got, want)
		}
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPostgresAStagedVectorIsNotUsedAfterTheProviderChanges(t *testing.T) {
	const rows = 200
	s := openTest(t, testConfig(t))
	seedPGStageTable(t, s, "ident", "docs", rows)
	ctx := t.Context()

	first := &pgStageRecorder{}
	first.inside = func(context.Context) error {
		if first.calls >= 2 {
			return errors.New("provider is down")
		}
		return nil
	}
	if _, err := s.Migrate(ctx, "ident", "docs", vectorizeBody(), first.embedder("test"), store.Incarnation{Version: 1}); err == nil {
		t.Fatal("a provider failure mid-backfill must fail the migration")
	}
	second := &pgStageRecorder{}
	other := store.Embedder{Identity: "other", Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		second.calls++
		second.texts = append(second.texts, texts...)
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{float32(i + 1), 2, 3, 4}
		}
		return out, nil
	}}
	after, err := s.Migrate(ctx, "ident", "docs", vectorizeBody(), other, store.Incarnation{Version: 1})
	if err != nil {
		t.Fatalf("re-issuing under a new provider identity must succeed: %v", err)
	}
	if second.embedded() != rows {
		t.Fatalf("a stage made under \"test\" was reused under \"other\": only %d of %d rows were embedded", second.embedded(), rows)
	}
	if after.EmbedSpace != "other" {
		t.Fatalf("the migration ended in embedding space %q, want other", after.EmbedSpace)
	}
}

func TestPostgresAStagedVectorIsNotUsedAfterTheTableIsRecreated(t *testing.T) {
	const rows = 200
	s := openTest(t, testConfig(t))
	seedPGStageTable(t, s, "recreate", "docs", rows)
	ctx := t.Context()

	first := &pgStageRecorder{}
	first.inside = func(context.Context) error {
		if first.calls >= 2 {
			return errors.New("provider is down")
		}
		return nil
	}
	if _, err := s.Migrate(ctx, "recreate", "docs", vectorizeBody(), first.embedder("test"), store.Incarnation{Version: 1}); err == nil {
		t.Fatal("a provider failure mid-backfill must fail the migration")
	}
	if stage := pgStageCount(t, s, "recreate", "docs"); stage == 0 {
		t.Fatal("the failed migration staged nothing, so this proves nothing about recreating the table")
	}
	if err := s.DropTable(ctx, "recreate", "docs", store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if stage := pgStageCount(t, s, "recreate", "docs"); stage != 0 {
		t.Fatalf("drop_table left %d staged rows behind, want none", stage)
	}
	if _, err := s.CreateTable(ctx, "recreate", "docs", []schema.Field{{Name: "body", Type: schema.Text}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "recreate", "docs", []map[string]any{{"body": "a brand new row"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	second := &pgStageRecorder{}
	after, err := s.Migrate(ctx, "recreate", "docs", vectorizeBody(), second.embedder("test"), store.Incarnation{Version: 1})
	if err != nil {
		t.Fatalf("migrating the recreated table must succeed: %v", err)
	}
	if after.Version != 2 {
		t.Fatalf("the recreated table ended at version %d, want 2", after.Version)
	}
	if second.embedded() != 1 {
		t.Fatalf("the recreated table embedded %d rows, want only its own 1: a stage from the dropped table must not carry over", second.embedded())
	}
}

func TestPostgresWritesDuringTheBackfillEndWithTheVectorOfTheirFinalText(t *testing.T) {
	for _, c := range []struct {
		name  string
		write func(ctx context.Context, s *Store) error
		id    int64
	}{
		{
			name: "inserted ahead of the cursor",
			write: func(ctx context.Context, s *Store) error {
				_, err := s.Insert(ctx, "wr", "docs", []map[string]any{{"body": "inserted mid-backfill"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{})
				return err
			},
			id: 301,
		},
		{
			name: "text updated behind the cursor",
			write: func(ctx context.Context, s *Store) error {
				_, err := s.Update(ctx, "wr", "docs", "id = 5", nil, map[string]any{"body": "row 5 rewritten behind the cursor"}, store.Embedder{}, nil, store.Incarnation{})
				return err
			},
			id: 5,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			const rows = 300
			s := openTest(t, testConfig(t))
			seedPGStageTable(t, s, "wr", "docs", rows)
			rec := &pgStageRecorder{}
			rec.inside = func(ctx context.Context) error {
				if rec.calls == 2 {
					return c.write(ctx, s)
				}
				return nil
			}
			if _, err := s.Migrate(t.Context(), "wr", "docs", vectorizeBody(), rec.embedder("test"), store.Incarnation{Version: 1}); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			if err := pgRowVectorsAreItsOwnText(t, s, "wr", "docs", c.id); err != nil {
				t.Error(err)
			}
		})
	}
}

func pgRowVectorsAreItsOwnText(t *testing.T, s *Store, ns, table string, id int64) error {
	t.Helper()
	rows, err := s.GetRows(t.Context(), ns, table, []int64{id}, nil, store.Incarnation{})
	if err != nil {
		return err
	}
	if len(rows.Rows) != 1 {
		return fmt.Errorf("row %d is not readable after the migration: %+v", id, rows.Rows)
	}
	text, _ := rows.Rows[0]["body"].(string)
	res, err := s.SearchVector(t.Context(), ns, table, store.VectorQuery{Vec: pgVectorOf(text)}, false, nil, store.Incarnation{}, store.Page{Limit: 1})
	if err != nil {
		return err
	}
	if len(res.Rows) != 1 || fmt.Sprint(res.Rows[0]["id"]) != fmt.Sprint(id) {
		return fmt.Errorf("row %d (%q) is not the nearest row to its own vector, so it carries some other text's vector: %+v", id, text, res.Rows)
	}
	return nil
}

func pgVectorOf(text string) []float32 {
	v := make([]float32, 8)
	for _, b := range []byte(text) {
		v[b%8]++
	}
	return v
}

func TestPostgresARowEmptiedDuringTheBackfillCarriesNoVector(t *testing.T) {
	const rows = 300
	s := openTest(t, testConfig(t))
	seedPGStageTable(t, s, "wr", "docs", rows)
	rec := &pgStageRecorder{}
	rec.inside = func(ctx context.Context) error {
		if rec.calls != 2 {
			return nil
		}
		_, err := s.Update(ctx, "wr", "docs", "id = 6", nil, map[string]any{"body": ""}, store.Embedder{}, nil, store.Incarnation{})
		return err
	}
	if _, err := s.Migrate(t.Context(), "wr", "docs", vectorizeBody(), rec.embedder("test"), store.Incarnation{Version: 1}); err != nil {
		t.Fatalf("emptying a row during the backfill must not stop the migration: %v", err)
	}
	if pgRowHasVector(t, s, "wr", "docs", 6) {
		t.Fatal("a row emptied after its page was staged still carries the vector of its old text")
	}
	if !pgRowHasVector(t, s, "wr", "docs", 7) {
		t.Fatal("a row that kept its text lost its vector")
	}
}

func pgRowHasVector(t *testing.T, s *Store, ns, table string, id int64) bool {
	t.Helper()
	var has bool
	if err := s.read(t.Context(), ns, func(tx pgx.Tx, nsp namespace) error {
		state, err := s.loadTable(t.Context(), tx, nsp, table)
		if err != nil {
			return err
		}
		return tx.QueryRow(t.Context(), `SELECT "_embedding" IS NOT NULL FROM `+ident(nsp.physical, state.physical)+` WHERE id = $1`, id).Scan(&has)
	}); err != nil {
		t.Fatal(err)
	}
	return has
}

func TestPostgresRenamingAndVectorizingInOneMigration(t *testing.T) {
	s := openTest(t, testConfig(t))
	seedPGStageTable(t, s, "ren", "docs", 3)
	ctx := t.Context()
	if _, err := s.Insert(ctx, "ren", "docs", []map[string]any{{"body": "alpha"}, {"body": "beta"}, {"body": "gamma"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	rec := &pgStageRecorder{}
	after, err := s.Migrate(ctx, "ren", "docs", []schema.Change{
		{Op: schema.OpRenameField, From: "body", To: "text"},
		{Op: schema.OpSetVectorize, Name: "text", Value: boolPtr(true)},
	}, rec.embedder("test"), store.Incarnation{Version: 1})
	if err != nil {
		t.Fatalf("renaming and vectorizing in one migration must work: the text is read from the old column and stamped after the rename: %v", err)
	}
	if after.Version != 2 || after.VectorizeField() == nil {
		t.Fatalf("the migration ended at version %d with no vectorized field: %+v", after.Version, after.Fields)
	}
	for id := int64(1); id <= 4; id++ {
		if !pgRowHasVector(t, s, "ren", "docs", id) {
			t.Fatalf("row %d carries no vector after a rename and a backfill in one migration", id)
		}
	}
}

func boolPtr(b bool) *bool { return &b }

func TestPostgresTheStageSurvivesAFailedProviderAcrossProcesses(t *testing.T) {
	const rows = 200
	cfg := testConfig(t)
	first := openTest(t, cfg)
	seedPGStageTable(t, first, "keep", "docs", rows)
	rec := &pgStageRecorder{}
	rec.inside = func(context.Context) error {
		if rec.calls >= 2 {
			return errors.New("provider is down")
		}
		return nil
	}
	if _, err := first.Migrate(t.Context(), "keep", "docs", vectorizeBody(), rec.embedder("test"), store.Incarnation{Version: 1}); err == nil {
		t.Fatal("a provider failure mid-backfill must fail the migration")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := openTest(t, cfg)
	if stage := pgStageCount(t, second, "keep", "docs"); stage != embedBackfillPage {
		t.Fatalf("a fresh process sees %d staged rows, want the %d the failed attempt left behind: the stage is in the catalog, not in memory", stage, embedBackfillPage)
	}
	again := &pgStageRecorder{}
	if _, err := second.Migrate(t.Context(), "keep", "docs", vectorizeBody(), again.embedder("test"), store.Incarnation{Version: 1}); err != nil {
		t.Fatalf("re-issuing from a second process must succeed: %v", err)
	}
	if again.embedded() != rows-embedBackfillPage {
		t.Fatalf("the second process embedded %d rows, want the %d already staged", again.embedded(), rows-embedBackfillPage)
	}
}

func TestPostgresVacuumClearsAStageNoMigrateIsRunning(t *testing.T) {
	const rows = 200
	s := openTest(t, testConfig(t))
	seedPGStageTable(t, s, "vac", "docs", rows)
	ctx := t.Context()
	rec := &pgStageRecorder{}
	rec.inside = func(context.Context) error {
		if rec.calls >= 2 {
			return errors.New("provider is down")
		}
		return nil
	}
	if _, err := s.Migrate(ctx, "vac", "docs", vectorizeBody(), rec.embedder("test"), store.Incarnation{Version: 1}); err == nil {
		t.Fatal("a provider failure mid-backfill must fail the migration")
	}
	if stage := pgStageCount(t, s, "vac", "docs"); stage == 0 {
		t.Fatal("nothing was staged, so this proves nothing about vacuum")
	}
	if _, err := s.Vacuum(ctx, "vac"); err != nil {
		t.Fatalf("vacuum: %v", err)
	}
	if stage := pgStageCount(t, s, "vac", "docs"); stage != 0 {
		t.Fatalf("vacuum left %d staged rows behind with no migrate running, want none", stage)
	}
}

func TestPostgresTheCatalogGainsItsStageRelationOnUpgrade(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body", Type: schema.Text}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"DROP TABLE " + s.relation("embed_stage"),
		"UPDATE " + s.relation("version") + " SET version = 7",
	} {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("shape the catalog as it was before the stage (%s): %v", stmt, err)
		}
	}
	if err := s.bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap over a catalog with no stage relation: %v", err)
	}
	var version int
	if err := s.pool.QueryRow(ctx, "SELECT version FROM "+s.relation("version")).Scan(&version); err != nil || version != catalogVersion {
		t.Fatalf("catalog version %d after bootstrap, want %d: %v", version, catalogVersion, err)
	}
	var staged int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM "+s.relation("embed_stage")).Scan(&staged); err != nil {
		t.Fatalf("the upgraded catalog has no usable stage relation: %v", err)
	}
	if _, err := s.Migrate(ctx, "app", "notes", vectorizeBody(), (&pgStageRecorder{}).embedder("test"), store.Incarnation{Version: 1}); err != nil {
		t.Fatalf("migrate over an upgraded catalog: %v", err)
	}
}

func TestPostgresTheEncodingIsReadForTheStagingGuard(t *testing.T) {
	s := openTest(t, testConfig(t))
	if enc := strings.ToUpper(s.serverEncoding); enc != "UTF8" && enc != "UTF-8" {
		t.Fatalf("the test database reports server_encoding %q, so the non-UTF8 guard is what this run is exercising", s.serverEncoding)
	}
	if err := s.requireUTF8ForStaging(); err != nil {
		t.Fatalf("a UTF-8 database must be allowed to vectorize: %v", err)
	}
	s.serverEncoding = "LATIN1"
	err := s.requireUTF8ForStaging()
	if err == nil {
		t.Fatal("a non-UTF-8 database must be refused, because convert_to would re-encode the text and no digest would ever match")
	}
	if !strings.Contains(err.Error(), "LATIN1") || !strings.Contains(err.Error(), "UTF8") {
		t.Fatalf("the refusal must name the encoding it found and the one it needs, got %v", err)
	}
	if !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("the refusal must be a classified invalid request, got %v", err)
	}
}

func TestPostgresANonUTF8DatabaseOnlyRefusesVectorizingMigrations(t *testing.T) {
	s := openTest(t, testConfig(t))
	seedPGStageTable(t, s, "enc", "docs", 2)
	ctx := t.Context()
	s.serverEncoding = "LATIN1"
	if _, err := s.Migrate(ctx, "enc", "docs", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "extra", Type: schema.Number}},
	}, store.Embedder{}, store.Incarnation{Version: 1}); err != nil {
		t.Fatalf("a migration that embeds nothing must still run on a non-UTF-8 database: %v", err)
	}
	if _, err := s.PlanMigration(ctx, "enc", "docs", vectorizeBody(), store.Embedder{}, store.Incarnation{Version: 2}, nil, store.Incarnation{}); err == nil {
		t.Fatal("a dry run that would vectorize must be refused on a non-UTF-8 database, or the plan would promise something the migrate cannot do")
	}
	if _, err := s.Migrate(ctx, "enc", "docs", vectorizeBody(), store.Embedder{}, store.Incarnation{Version: 2}); err == nil {
		t.Fatal("a vectorizing migrate must be refused on a non-UTF-8 database")
	}
}

func TestPostgresAStageIsNotStampedWhenTheProviderChangesDimension(t *testing.T) {
	const rows = 200
	s := openTest(t, testConfig(t))
	seedPGStageTable(t, s, "dims", "docs", rows)
	ctx := t.Context()

	first := &pgStageRecorder{}
	first.inside = func(context.Context) error {
		if first.calls >= 2 {
			return errors.New("provider is down")
		}
		return nil
	}
	if _, err := s.Migrate(ctx, "dims", "docs", vectorizeBody(), first.embedder("test"), store.Incarnation{Version: 1}); err == nil {
		t.Fatal("a provider failure mid-backfill must fail the migration")
	}
	if stage := pgStageCount(t, s, "dims", "docs"); stage != embedBackfillPage {
		t.Fatalf("the first attempt staged %d rows, want the one page it finished", stage)
	}
	embedded := 0
	narrow := store.Embedder{Identity: "test", Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		embedded += len(texts)
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 2, 3, 4}
		}
		return out, nil
	}}
	_, err := s.Migrate(ctx, "dims", "docs", vectorizeBody(), narrow, store.Incarnation{Version: 1})
	if err == nil {
		t.Fatal("a provider that reports one identity and two dimensions must not activate")
	}
	if !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("the refusal must be classified like SQLite's, not as a server fault, got %v", err)
	}
	if !strings.Contains(err.Error(), "different lengths") {
		t.Fatalf("the refusal must name the mixed lengths, got %v", err)
	}
	if embedded == 0 {
		t.Fatal("the second attempt embedded nothing, so the mismatch was never reached")
	}
	if live := pgLiveEmbeddings(t, s, "dims", "docs"); live != 0 {
		t.Fatalf("%d rows were stamped with a vector of the wrong length", live)
	}
	sc, _, err := s.DescribeTable(ctx, "dims", "docs", nil, store.Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	if sc.Version != 1 {
		t.Fatalf("a refused stamp left the table at version %d, want 1", sc.Version)
	}
	log, err := s.ListMigrations(ctx, "dims", "docs", store.Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 0 {
		t.Fatalf("a refused stamp recorded %d migrations, want none", len(log))
	}
}

func TestPostgresAProviderThatChangesDimensionMidAttemptIsCaughtBeforeStaging(t *testing.T) {
	const rows = 300
	s := openTest(t, testConfig(t))
	seedPGStageTable(t, s, "mid", "docs", rows)
	ctx := t.Context()
	calls := 0
	shifting := store.Embedder{Identity: "test", Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		calls++
		dim := 8
		if calls > 1 {
			dim = 4
		}
		out := make([][]float32, len(texts))
		for i, text := range texts {
			v := make([]float32, dim)
			for _, b := range []byte(text) {
				v[int(b)%dim]++
			}
			out[i] = v
		}
		return out, nil
	}}
	_, err := s.Migrate(ctx, "mid", "docs", vectorizeBody(), shifting, store.Incarnation{Version: 1})
	if err == nil {
		t.Fatal("a provider that changes dimension between two pages of one attempt must not be allowed to stage a mixed-length stage")
	}
	if !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("the refusal must be classified as an invalid request, got %v", err)
	}
	if !strings.Contains(err.Error(), "4-dimensional") {
		t.Fatalf("the refusal must name the dimension the provider switched to, got %v", err)
	}
	if stage := pgStageCount(t, s, "mid", "docs"); stage != embedBackfillPage {
		t.Fatalf("%d rows are staged, want only the first page's %d: the second page must be refused before it is written", stage, embedBackfillPage)
	}
	if live := pgLiveEmbeddings(t, s, "mid", "docs"); live != 0 {
		t.Fatalf("%d rows were stamped from a mixed-length stage", live)
	}
}

func TestPostgresAMixedLengthStageIsRecoverableByTurningVectorizeOff(t *testing.T) {
	const rows = 200
	s := openTest(t, testConfig(t))
	seedPGStageTable(t, s, "wedge", "docs", rows)
	ctx := t.Context()

	first := &pgStageRecorder{}
	first.inside = func(context.Context) error {
		if first.calls >= 2 {
			return errors.New("provider is down")
		}
		return nil
	}
	if _, err := s.Migrate(ctx, "wedge", "docs", vectorizeBody(), first.embedder("test"), store.Incarnation{Version: 1}); err == nil {
		t.Fatal("a provider failure mid-backfill must fail the migration")
	}
	narrow := store.Embedder{Identity: "test", Embed: func(ctx context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, text := range texts {
			v := make([]float32, 4)
			for _, b := range []byte(text) {
				v[int(b)%4]++
			}
			out[i] = v
		}
		return out, nil
	}}
	if _, err := s.Migrate(ctx, "wedge", "docs", vectorizeBody(), narrow, store.Incarnation{Version: 1}); err == nil {
		t.Fatal("a stage holding two lengths must refuse to activate")
	} else if !strings.Contains(err.Error(), "re-embed via migrate") {
		t.Fatalf("the refusal must name its own remedy, so a caller can follow it: %v", err)
	}
	if stage := pgStageCount(t, s, "wedge", "docs"); stage == 0 {
		t.Fatal("nothing is staged, so this proves nothing about recovering from a mixed-length stage")
	}
	off := false
	if _, err := s.Migrate(ctx, "wedge", "docs", []schema.Change{
		{Op: schema.OpSetVectorize, Name: "body", Value: &off},
	}, store.Embedder{}, store.Incarnation{Version: 1}); err != nil {
		t.Fatalf("turning vectorize off must succeed, it is the remedy the refusal names: %v", err)
	}
	if stage := pgStageCount(t, s, "wedge", "docs"); stage != 0 {
		t.Fatalf("the remedy the refusal names left %d staged rows behind, so a caller following it stays wedged", stage)
	}
	after, err := s.Migrate(ctx, "wedge", "docs", vectorizeBody(), (&pgStageRecorder{}).embedder("test"), store.Incarnation{Version: 2})
	if err != nil {
		t.Fatalf("re-embedding after the remedy must succeed: %v", err)
	}
	if after.EmbedDim != 8 || after.Version != 3 {
		t.Fatalf("the re-embedded table ended at version %d dim %d, want 3 and 8", after.Version, after.EmbedDim)
	}
	if live := pgLiveEmbeddings(t, s, "wedge", "docs"); live != rows {
		t.Fatalf("%d rows carry a vector after the re-embed, want %d", live, rows)
	}
}

func TestAPlanInOneNamespaceDoesNotCountAnotherNamespacesStagedRows(t *testing.T) {
	s := openTest(t, testConfig(t))
	seedPGStageTable(t, s, "alpha", "docs", 200)
	ctx := t.Context()
	first := &pgStageRecorder{}
	first.inside = func(context.Context) error {
		if first.calls >= 2 {
			return errors.New("provider is down")
		}
		return nil
	}
	if _, err := s.Migrate(ctx, "alpha", "docs", vectorizeBody(), first.embedder("test"), store.Incarnation{Version: 1}); err == nil {
		t.Fatal("a provider failure mid-backfill must fail the migration")
	}
	staged := pgStageCount(t, s, "alpha", "docs")
	if staged == 0 {
		t.Fatal("nothing was staged in alpha, so this proves nothing about the other namespace")
	}
	seedPGStageTable(t, s, "beta", "docs", 40)
	plan, err := s.PlanMigration(ctx, "beta", "docs", vectorizeBody(), (&pgStageRecorder{}).embedder("test"), store.Incarnation{Version: 1}, nil, store.Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.StagedRows != 0 {
		t.Fatalf("beta's plan reports staged_rows %d, but the %d staged rows belong to alpha", plan.StagedRows, staged)
	}
	if plan.EmbedRows != 40 {
		t.Fatalf("beta's plan reports embed_rows %d, want its own 40", plan.EmbedRows)
	}
	alpha, err := s.PlanMigration(ctx, "alpha", "docs", vectorizeBody(), (&pgStageRecorder{}).embedder("test"), store.Incarnation{Version: 1}, nil, store.Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	if alpha.StagedRows != staged {
		t.Fatalf("alpha's plan reports staged_rows %d, want its own %d", alpha.StagedRows, staged)
	}
	if alpha.EmbedRows != 200-staged {
		t.Fatalf("alpha's plan reports embed_rows %d, want the %d rows it still needs", alpha.EmbedRows, 200-staged)
	}
}

func TestAPlanCountsOnlyTheStagedRowsWhoseTextStillMatches(t *testing.T) {
	const rows = 200
	s := openTest(t, testConfig(t))
	seedPGStageTable(t, s, "stale", "docs", rows)
	ctx := t.Context()
	first := &pgStageRecorder{}
	first.inside = func(context.Context) error {
		if first.calls >= 2 {
			return errors.New("provider is down")
		}
		return nil
	}
	if _, err := s.Migrate(ctx, "stale", "docs", vectorizeBody(), first.embedder("test"), store.Incarnation{Version: 1}); err == nil {
		t.Fatal("a provider failure mid-backfill must fail the migration")
	}
	staged := pgStageCount(t, s, "stale", "docs")
	if _, err := s.Update(ctx, "stale", "docs", "id = 1", nil, map[string]any{"body": "row 1 was rewritten after it was staged"}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete(ctx, "stale", "docs", "id = 2", nil, store.DeleteOpts{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	plan, err := s.PlanMigration(ctx, "stale", "docs", vectorizeBody(), (&pgStageRecorder{}).embedder("test"), store.Incarnation{Version: 1}, nil, store.Incarnation{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.StagedRows != staged-2 {
		t.Fatalf("the plan reports staged_rows %d, want %d: the rewritten row and the deleted one are not reusable", plan.StagedRows, staged-2)
	}
	if plan.EmbedRows != (rows-1)-(staged-2) {
		t.Fatalf("the plan reports embed_rows %d, want %d", plan.EmbedRows, (rows-1)-(staged-2))
	}
}
