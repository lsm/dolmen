package conformance

import (
	"errors"
	"fmt"
	"testing"
)

const resumeRows = 300

// resumeFailAt names a row the fake provider refuses to embed, so a backfill is interrupted part
// way through: the pages before it are embedded and kept, the page holding it fails whole. The
// server batches texts on their way to the provider, so this lands on a page boundary whatever
// the batch size is, and both engines stage the same pages before it.
const resumeFailAt = "row 260 of the backfill fixture"

func backfillText(i int) string { return fmt.Sprintf("row %d of the backfill fixture", i) }

func seedBackfillRows(t *testing.T, h *harness, ns, table string, rows int) {
	t.Helper()
	records := make([]map[string]any, 0, rows)
	for i := 0; i < rows; i++ {
		records = append(records, map[string]any{"body": backfillText(i)})
	}
	for start := 0; start < len(records); start += 100 {
		end := min(start+100, len(records))
		h.mustHTTP("insert", map[string]any{
			"namespace": ns, "table": table, "records": records[start:end],
		})
	}
}

func vectorizeBody() []map[string]any {
	return []map[string]any{{"op": "set_vectorize", "name": "body", "value": true}}
}

func seedUnvectorizedBackfill(t *testing.T, h *harness, ns string) {
	t.Helper()
	h.seedTable(ns, "docs", []map[string]any{{"name": "body", "type": "text"}})
	seedBackfillRows(t, h, ns, "docs", resumeRows)
}

// interruptBackfill fails the migration part way through, and reports how many rows the provider
// was asked for before it gave up: those are the rows whose vectors are kept.
func interruptBackfill(t *testing.T, h *harness, ns string) int {
	t.Helper()
	h.emb.failOnText(resumeFailAt, errors.New("provider is down"))
	status, body := h.httpCall("migrate", map[string]any{
		"namespace": ns, "table": "docs", "expected_version": 1, "changes": vectorizeBody(),
	})
	if status < 400 {
		t.Fatalf("a provider that fails part way through a backfill must fail the migration, got %d: %v", status, body)
	}
	asked := h.emb.embeddedTexts()
	if len(asked) == 0 || len(asked) >= resumeRows {
		t.Fatalf("the interrupted migration asked the provider for %d of %d rows, want some but not all", len(asked), resumeRows)
	}
	for i, text := range asked {
		if want := backfillText(i); text != want {
			t.Fatalf("the interrupted migration asked for %q at position %d, want %q: the backfill walks the table in order", text, i, want)
		}
	}
	h.emb.forgetTexts()
	return len(asked)
}

func planOf(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	p, ok := out["plan"].(map[string]any)
	if !ok {
		t.Fatalf("a dry run must report its plan: %v", out)
	}
	return p
}

func TestAPlanReportsHowMuchOfAVectorizeIsAlreadyStaged(t *testing.T) {
	h := newHarness(t)
	seedUnvectorizedBackfill(t, h, "staged")

	first := planOf(t, h.mustHTTP("migrate", map[string]any{
		"namespace": "staged", "table": "docs", "expected_version": 1,
		"dry_run": true, "changes": vectorizeBody(),
	}))
	if got := int64val(t, "staged_rows", first["staged_rows"]); got != 0 {
		t.Fatalf("a migration that has not run reports staged_rows %d, want 0", got)
	}
	if got := int64val(t, "embed_rows", first["embed_rows"]); got != resumeRows {
		t.Fatalf("a migration that has not run reports embed_rows %d, want the whole table's %d", got, resumeRows)
	}

	asked := interruptBackfill(t, h, "staged")
	second := planOf(t, h.mustHTTP("migrate", map[string]any{
		"namespace": "staged", "table": "docs", "expected_version": 1,
		"dry_run": true, "changes": vectorizeBody(),
	}))
	staged := int64val(t, "staged_rows", second["staged_rows"])
	embed := int64val(t, "embed_rows", second["embed_rows"])
	if staged != int64(asked) {
		t.Fatalf("the plan reports staged_rows %d, want the %d rows the provider was asked for before it failed", staged, asked)
	}
	if embed != int64(resumeRows-asked) {
		t.Fatalf("the plan reports embed_rows %d, want the %d rows still needing a call", embed, resumeRows-asked)
	}
	if staged+embed != resumeRows {
		t.Fatalf("staged_rows %d plus embed_rows %d is %d, want the table's whole %d", staged, embed, staged+embed, resumeRows)
	}
}

func TestAReissuedVectorizeEmbedsOnlyWhatTheFailedAttemptDidNot(t *testing.T) {
	h := newHarness(t)
	seedUnvectorizedBackfill(t, h, "resume")
	asked := interruptBackfill(t, h, "resume")

	out := h.mustHTTP("migrate", map[string]any{
		"namespace": "resume", "table": "docs", "expected_version": 1, "changes": vectorizeBody(),
	})
	if table := out["table"].(map[string]any); int64val(t, "version", table["version"]) != 2 {
		t.Fatalf("the re-issued migration left the table at %v, want version 2", table["version"])
	}
	again := h.emb.embeddedTexts()
	if len(again) != resumeRows-asked {
		t.Fatalf("the re-issued migration asked the provider for %d rows, want only the %d it had not already embedded", len(again), resumeRows-asked)
	}
	for i, text := range again {
		if want := backfillText(asked + i); text != want {
			t.Fatalf("the re-issued migration asked for %q at position %d, want %q: it must skip the rows already embedded", text, i, want)
		}
	}
	hits := h.mustHTTP("search_vector", map[string]any{
		"namespace": "resume", "table": "docs", "text": backfillText(0), "limit": resumeRows + 1,
	})
	if skipped := int64val(t, "skipped_vectors", hits["skipped_vectors"]); skipped != 0 {
		t.Fatalf("%d rows hold no vector after the re-issued migration, so the table is only partly embedded", skipped)
	}
}

func TestTheStagedPlanIsTheSameOverEveryTransport(t *testing.T) {
	for _, tr := range []struct {
		name string
		call func(h *harness, args map[string]any) map[string]any
	}{
		{name: "http", call: func(h *harness, args map[string]any) map[string]any { return h.mustHTTP("migrate", args) }},
		{name: "mcp", call: func(h *harness, args map[string]any) map[string]any { return h.mustMCP("migrate", args) }},
	} {
		t.Run(tr.name, func(t *testing.T) {
			h := newHarness(t)
			seedUnvectorizedBackfill(t, h, "tr")
			asked := interruptBackfill(t, h, "tr")

			plan := planOf(t, tr.call(h, map[string]any{
				"namespace": "tr", "table": "docs", "expected_version": 1,
				"dry_run": true, "changes": vectorizeBody(),
			}))
			if got := int64val(t, "staged_rows", plan["staged_rows"]); got != int64(asked) {
				t.Fatalf("the plan reports staged_rows %d, want %d", got, asked)
			}
			if got := int64val(t, "embed_rows", plan["embed_rows"]); got != int64(resumeRows-asked) {
				t.Fatalf("the plan reports embed_rows %d, want %d", got, resumeRows-asked)
			}

			out := tr.call(h, map[string]any{
				"namespace": "tr", "table": "docs", "expected_version": 1, "changes": vectorizeBody(),
			})
			if table := out["table"].(map[string]any); int64val(t, "version", table["version"]) != 2 {
				t.Fatalf("the re-issued migration left the table at %v, want version 2", table["version"])
			}
		})
	}
}
