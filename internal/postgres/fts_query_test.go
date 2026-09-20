package postgres

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/store"
)

func TestCompileFTSQueryRejectsUnsupportedGrammar(t *testing.T) {
	for _, tc := range []struct {
		match string
		want  string
	}{
		{"title:payment", "column filter"},
		{"{title body}:payment", "column-group filter"},
		{"NEAR(payment refund)", "proximity"},
		{"^gateway", "first-token"},
		{"payment ^gateway", "first-token"},
		{"a + b", "adjacency"},
		{"a+b", "adjacency"},
		{"can't", "single quotes"},
		{`"unterminated`, "unterminated"},
		{"(payment", "unbalanced"},
		{"payment)", "unbalanced"},
		{"", "no search terms"},
		{"OR payment", "expected a search term"},
		{"payment AND", "expected a search term"},
	} {
		_, _, err := compileFTSQuery(tc.match, 0)
		if !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("%q: want invalid, got %v", tc.match, err)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%q: want message containing %q, got %v", tc.match, tc.want, err)
		}
	}
}

func TestCompileFTSQueryShape(t *testing.T) {
	for _, tc := range []struct {
		match string
		args  []any
		sql   string
	}{
		{"payment", []any{"payment"}, "plainto_tsquery('english',$1)"},
		{"pay*", []any{"'pay':*"}, "to_tsquery('english',$1)"},
		{`"foo bar"`, []any{"foo bar"}, "phraseto_tsquery('english',$1)"},
		{"a b", []any{"a", "b"}, "(plainto_tsquery('english',$1) && plainto_tsquery('english',$2))"},
		{"a AND b", []any{"a", "b"}, "(plainto_tsquery('english',$1) && plainto_tsquery('english',$2))"},
		{"a OR b", []any{"a", "b"}, "(plainto_tsquery('english',$1) || plainto_tsquery('english',$2))"},
		{"a NOT b", []any{"a", "b"}, "(plainto_tsquery('english',$1) && !! plainto_tsquery('english',$2))"},
	} {
		sql, args, err := compileFTSQuery(tc.match, 0)
		if err != nil {
			t.Fatalf("%q: %v", tc.match, err)
		}
		if sql != tc.sql {
			t.Fatalf("%q: sql = %q, want %q", tc.match, sql, tc.sql)
		}
		if len(args) != len(tc.args) {
			t.Fatalf("%q: args = %+v, want %+v", tc.match, args, tc.args)
		}
		for i := range args {
			if args[i] != tc.args[i] {
				t.Fatalf("%q: args = %+v, want %+v", tc.match, args, tc.args)
			}
		}
	}
}

func TestCompileFTSQueryPrecedence(t *testing.T) {
	sql, _, err := compileFTSQuery("a OR b AND c", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sql, "(plainto_tsquery('english',$1) || (") {
		t.Fatalf("AND must bind tighter than OR: %s", sql)
	}
	sql, _, err = compileFTSQuery("a NOT b AND c", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "!!") || !strings.HasPrefix(sql, "((") {
		t.Fatalf("NOT must bind tighter than AND: %s", sql)
	}
}

func TestCompileFTSQueryParameterOffset(t *testing.T) {
	sql, args, err := compileFTSQuery("a OR b", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "$4") || !strings.Contains(sql, "$5") {
		t.Fatalf("offset ignored: %s", sql)
	}
	if len(args) != 2 {
		t.Fatalf("args = %+v", args)
	}
}

func TestCompileFTSQueryTreatsMetacharactersAsText(t *testing.T) {
	cfg := testConfig(t)
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, cfg.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, match := range []string{"a | b & c", `\\backslash`, "cat*", `"a | b"`, "a OR (b NOT c)"} {
		sql, args, err := compileFTSQuery(match, 0)
		if err != nil {
			t.Fatalf("%q: %v", match, err)
		}
		var rendered string
		if err := conn.QueryRow(ctx, "SELECT ("+sql+")::text", args...).Scan(&rendered); err != nil {
			t.Fatalf("%q compiled to SQL PostgreSQL rejected: %v (%s)", match, err, sql)
		}
		if match == "a | b & c" && rendered != "'b' & 'c'" {
			t.Fatalf("%q must become literal lexemes, got %s", match, rendered)
		}
	}
}
