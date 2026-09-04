package store

import (
	"context"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

// Porter stemming is the default for fulltext indexes (#147): plural and
// inflected query terms match their indexed singulars, phrases and prefix
// terms operate on stems, and the derivation boundary porter does not cross
// (suffix-stripper, not lemmatizer) stays as documented.
func TestFulltextStemming(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "stems", []schema.Field{
		{Name: "body", Type: schema.Text, Fulltext: true},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "stems", []map[string]any{
		{"body": "the payments were refunded"},
		{"body": "network latency spike"},
		{"body": "付款网关已退款"},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// The issue's core trap: inflected queries match the indexed singular.
	for _, q := range []string{"payment", "payments", "refund", "refunds", "refunded"} {
		rows, _, err := st.SearchFulltext(ctx, "test", "stems", q, 0, 10, false, "", nil)
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		if len(rows) != 1 {
			t.Fatalf("stemmed query %q must match the singular row: %v", q, rows)
		}
	}

	// Phrases match on stems: each word is stemmed before adjacency matching.
	rows, _, err := st.SearchFulltext(ctx, "test", "stems", `"payments were"`, 0, 10, false, "", nil)
	if err != nil {
		t.Fatalf("phrase on stems: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf(`stemmed phrase "payments were" must match: %v`, rows)
	}

	// Prefix queries operate on stems: pay* stems to pai*, so it does not
	// reach payment (whose stem is payment) — and neither does the inflected
	// "paying" (stem pai), because porter strips suffixes, not derivations.
	for _, q := range []string{"pay*", "paying"} {
		rows, _, err := st.SearchFulltext(ctx, "test", "stems", q, 0, 10, false, "", nil)
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		if len(rows) != 0 {
			t.Fatalf("query %q must not match payment (porter stems pay/paying to pai): %v", q, rows)
		}
	}
	rows, _, err = st.SearchFulltext(ctx, "test", "stems", "payment*", 0, 10, false, "", nil)
	if err != nil {
		t.Fatalf("search payment*: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("prefix over payment's own stem must match: %v", rows)
	}

	// Stemming is English-focused: an uninterrupted CJK run is untouched by
	// the stemmer and still indexed as one opaque token (#106) — whole-run
	// terms match, interior runs silently do not.
	rows, _, err = st.SearchFulltext(ctx, "test", "stems", "付款网关已退款", 0, 10, false, "", nil)
	if err != nil {
		t.Fatalf("search CJK run: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("whole-run CJK term must match: %v", rows)
	}
	rows, _, err = st.SearchFulltext(ctx, "test", "stems", "付款网关", 0, 10, false, "", nil)
	if err != nil {
		t.Fatalf("search CJK prefix run: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("interior CJK run must not match, same as before stemming: %v", rows)
	}
}

// Tables indexed before stemming became the default keep their exact-token
// index and keep working; re-asserting set_fulltext = true rebuilds the index
// under the engine's current tokenizer (#147).
func TestFulltextReindexViaSetFulltext(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.CreateTable(ctx, "test", "legacy", []schema.Field{
		{Name: "body", Type: schema.Text, Fulltext: true},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.Insert(ctx, "test", "legacy", []map[string]any{
		{"body": "payment gateway refunded"},
	}, testEmbed); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Rewind the shadow index to the pre-stemming exact-token format.
	n, err := st.ns("test")
	if err != nil {
		t.Fatalf("ns: %v", err)
	}
	if _, err := n.rw.ExecContext(ctx, `DROP TABLE IF EXISTS "legacy__fts"`); err != nil {
		t.Fatalf("drop fts: %v", err)
	}
	if _, err := n.rw.ExecContext(ctx, `CREATE VIRTUAL TABLE "legacy__fts" USING fts5("body")`); err != nil {
		t.Fatalf("create legacy fts: %v", err)
	}
	if _, err := n.rw.ExecContext(ctx,
		`INSERT INTO "legacy__fts"(rowid, body) SELECT id, body FROM "legacy" WHERE body IS NOT NULL`); err != nil {
		t.Fatalf("populate legacy fts: %v", err)
	}

	// The old index keeps working — exact tokens still match...
	rows, _, err := st.SearchFulltext(ctx, "test", "legacy", "payment", 0, 10, false, "", nil)
	if err != nil {
		t.Fatalf("exact-token search: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("exact-token index must keep matching: %v", rows)
	}
	// ...while inflected queries miss, as before the change.
	rows, _, err = st.SearchFulltext(ctx, "test", "legacy", "payments", 0, 10, false, "", nil)
	if err != nil {
		t.Fatalf("inflected search on old index: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("exact-token index must not stem: %v", rows)
	}

	// The dry-run plan reports the rebuild without applying it.
	plan, err := st.PlanMigration(ctx, "test", "legacy", []schema.Change{
		{Op: schema.OpSetFulltext, Name: "body", Value: boolPtr(true)},
	}, testEmbed, 1)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !plan.RebuildFulltext || plan.FulltextReindexRows != 1 {
		t.Fatalf("plan must report the reindex: rebuild=%v rows=%d", plan.RebuildFulltext, plan.FulltextReindexRows)
	}
	rows, _, err = st.SearchFulltext(ctx, "test", "legacy", "payments", 0, 10, false, "", nil)
	if err != nil {
		t.Fatalf("plan must not apply: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("dry-run must leave the old index in place: %v", rows)
	}

	// Re-asserting fulltext = true rebuilds under the current tokenizer.
	sc, err := st.Migrate(ctx, "test", "legacy", []schema.Change{
		{Op: schema.OpSetFulltext, Name: "body", Value: boolPtr(true)},
	}, testEmbed, 1)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if sc.Version != 2 {
		t.Fatalf("expected version 2, got %d", sc.Version)
	}
	for _, q := range []string{"payments", "refunds"} {
		rows, _, err := st.SearchFulltext(ctx, "test", "legacy", q, 0, 10, false, "", nil)
		if err != nil {
			t.Fatalf("search %q after reindex: %v", q, err)
		}
		if len(rows) != 1 {
			t.Fatalf("reindexed table must match inflected query %q: %v", q, rows)
		}
	}
}
