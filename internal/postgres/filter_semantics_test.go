package postgres

import (
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/filter"

	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestPostgresACrossClassComparisonStillPropagatesNull(t *testing.T) {
	cfg := testConfig(t)
	cfg.SharedFilter = true
	s := openTest(t, cfg)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	fields := []schema.Field{{Name: "body", Type: schema.Text}, {Name: "tag", Type: schema.Text}}
	if _, err := s.CreateTable(ctx, "app", "notes", fields, store.TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	records := []map[string]any{{"tag": "only"}}
	if _, err := s.Insert(ctx, "app", "notes", records, store.WriteOpts{Owner: "alice"}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		filter string
		why    string
	}{
		{"body > 1", "text sorts after a number, but a null body compares to null, not to true"},
		{"body = 1", "text never equals a number, and a null body is still null rather than false"},
		{"NOT (body = 1)", "NOT null is null, so a null body must not be matched by the negation either"},
	} {
		t.Run(tc.filter, func(t *testing.T) {
			scope := &store.RowScope{Owner: "alice"}
			result, err := s.Delete(ctx, "app", "notes", tc.filter, nil, store.DeleteOpts{DryRun: true}, scope, store.Incarnation{})
			if err != nil {
				t.Fatalf("the engine owes this filter an evaluation: %v", err)
			}
			if result.Matched != 0 {
				t.Fatalf("%s matched %d rows: %s", tc.filter, result.Matched, tc.why)
			}
		})
	}
}

func TestPostgresABoundTextArgumentCarriesTheBinaryCollation(t *testing.T) {
	cols := map[string]string{"body": "body"}
	types := map[string]schema.FieldType{"body": schema.Text}
	for _, tc := range []struct {
		expr string
		args []any
	}{
		{"? > 'B'", []any{"a"}},
		{"'B' < ?", []any{"a"}},
		{"body > ?", []any{"a"}},
	} {
		node, err := filter.Parse(tc.expr, filter.Options{Columns: []string{"body"}, Args: tc.args})
		if err != nil {
			t.Fatalf("%s: %v", tc.expr, err)
		}
		sql, _, err := renderScopedFilter(node, cols, types, tc.args, 1)
		if err != nil {
			t.Fatalf("%s: %v", tc.expr, err)
		}
		if !strings.Contains(sql, `COLLATE "C"`) {
			t.Fatalf("%s rendered as %s, which compares text in whatever collation the database was created with; SQLite's order is byte-wise and a bound argument is still text", tc.expr, sql)
		}
	}
}

func TestPostgresATextColumnRefusesAComputedNumberRatherThanGuessingItsText(t *testing.T) {
	cols := map[string]string{"code": "code", "n": "n"}
	types := map[string]schema.FieldType{"code": schema.Text, "n": schema.Number}
	for _, expr := range []string{"code = round(2.5)", "code > length(code)", "abs(1) = code",
		"code = (n + 100)", "(n * 1.5) > code", "code < -(n + 1)", "code = iif(n > 1, n + 1, n - 1)",
		"code = coalesce(n + 1, 2)"} {
		node, err := filter.Parse(expr, filter.Options{Columns: []string{"code", "n"}})
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		sql, _, err := renderScopedFilter(node, cols, types, nil, 1)
		if err == nil {
			t.Fatalf("%s rendered as %s rather than being refused, so the formatting gap has closed: pin the answer SQLite gives, and delete the paragraph in docs/design/postgresql.md that explains why a computed number is refused, in this same change. SQLite compares a text column against the double's own text, which PostgreSQL's numeric formatting does not reproduce, so answering without reproducing it is a wrong row set rather than a refusal", expr, sql)
		}
	}
}

func TestPostgresATextClassNamesItsCharactersRatherThanAskingTheLocale(t *testing.T) {
	cols := map[string]string{"body": "body", "n": "n"}
	types := map[string]schema.FieldType{"body": schema.Text, "n": schema.Number}
	for _, tc := range []struct {
		expr string
		args []any
	}{
		{"n = ?", []any{" -7"}},
		{"(n % ?) IS NULL", []any{" -7"}},
		{"abs(body) = 0", nil},
		{"body + 1 = 1", nil},
		{"n = body", nil},
	} {
		node, err := filter.Parse(tc.expr, filter.Options{Columns: []string{"body", "n"}, Args: tc.args})
		if err != nil {
			t.Fatalf("%s: %v", tc.expr, err)
		}
		sql, _, err := renderScopedFilter(node, cols, types, tc.args, 1)
		if err != nil {
			t.Fatalf("%s: %v", tc.expr, err)
		}
		if strings.Contains(sql, "[:space:]") {
			t.Fatalf("%s rendered a POSIX space class, which follows the server's locale: on a UTF-8 lc_ctype it matches U+00A0 and U+2002, which SQLite does not skip, so a numeral behind one converts here and stays text there. The Go side spells the six characters SQLite skips; the SQL side has to spell the same six. Rendered: %s", tc.expr, sql)
		}
	}
}

func TestPostgresRefusesAFilterChoosingAmongTooManyValuesOfDifferentTypes(t *testing.T) {
	cols := map[string]string{"body": "body", "n": "n"}
	types := map[string]schema.FieldType{"body": schema.Text, "n": schema.Number}
	render := func(count int) error {
		terms := make([]string, count)
		for i := range terms {
			terms[i] = "coalesce(body, 1) = body"
		}
		expr := strings.Join(terms, " AND ")
		node, err := filter.Parse(expr, filter.Options{Columns: []string{"body", "n"}})
		if err != nil {
			t.Fatalf("parse %q: %v", expr, err)
		}
		_, _, err = renderScopedFilter(node, cols, types, nil, 1)
		return err
	}
	if err := render(5); err != nil {
		t.Fatalf("five mixed coalesces need 62 copies, within the limit, but rendering failed: %v", err)
	}
	err := render(6)
	if err == nil || !strings.Contains(err.Error(), "choosing among this many values of different types") {
		t.Fatalf("six mixed coalesces need 126 copies and must be refused, got %v", err)
	}
}
