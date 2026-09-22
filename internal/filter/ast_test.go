package filter

import (
	"strings"
	"testing"
)

func parseOne(t *testing.T, expr string, args ...any) Node {
	t.Helper()
	node, err := Parse(expr, Options{Columns: tableColumns, Args: args})
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	if node == nil {
		t.Fatalf("Parse(%q) returned no tree", expr)
	}
	return node
}

func TestEveryAcceptedExpressionParsesToOneTree(t *testing.T) {
	for _, expr := range acceptedExpressions {
		args := []any{}
		for i := 0; i < strings.Count(expr, "?"); i++ {
			args = append(args, "x")
		}
		if strings.Contains(expr, "ESCAPE ?") {
			args[len(args)-1] = "!"
		}
		node, err := Parse(expr, Options{Columns: tableColumns, Args: args})
		if err != nil {
			t.Fatalf("Parse(%q) rejected a filter the allowlist permits: %v", expr, err)
		}
		if node == nil {
			t.Fatalf("Parse(%q) validated but built no tree; a renderer would have nothing to read", expr)
		}
	}
}

func TestTheTreeKeepsTheShapeOfTheExpression(t *testing.T) {
	binary, ok := parseOne(t, `score > 3 AND title = ?`, "x").(*Binary)
	if !ok || strings.ToLower(binary.Op) != "and" {
		t.Fatalf("the root of a conjunction is the conjunction: %#v", binary)
	}
	if _, ok := binary.Left.(*Binary); !ok {
		t.Fatalf("the left of AND is its own comparison: %#v", binary.Left)
	}
	right, ok := binary.Right.(*Binary)
	if !ok {
		t.Fatalf("the right of AND is its own comparison: %#v", binary.Right)
	}
	if col, ok := right.Left.(*Column); !ok || col.Name != "title" {
		t.Fatalf("a column reference survives as a column: %#v", right.Left)
	}
	if param, ok := right.Right.(*Param); !ok || param.Index != 0 {
		t.Fatalf("a bound argument keeps its position: %#v", right.Right)
	}
}

func TestTheTreeKeepsEveryPartOfTheHarderForms(t *testing.T) {
	call, ok := parseOne(t, `substr(title, 2, 3) = 'abc'`).(*Binary)
	if !ok {
		t.Fatal("expected a comparison at the root")
	}
	fn, ok := call.Left.(*Call)
	if !ok || fn.Name != "substr" || len(fn.Args) != 3 {
		t.Fatalf("a call keeps its name and every argument: %#v", call.Left)
	}

	in, ok := parseOne(t, `score IN (1, 2, 3)`).(*In)
	if !ok || in.Negated || len(in.List) != 3 {
		t.Fatalf("an IN keeps its list: %#v", in)
	}
	if _, ok := parseOne(t, `score NOT IN ()`).(*In); !ok {
		t.Fatal("an empty NOT IN is still an IN")
	}

	like, ok := parseOne(t, `title LIKE '100!%' ESCAPE '!'`).(*Like)
	if !ok || like.Escape == nil {
		t.Fatalf("an ESCAPE is part of the LIKE, not lost beside it: %#v", like)
	}

	between, ok := parseOne(t, `score NOT BETWEEN 1 AND 10`).(*Between)
	if !ok || !between.Negated || between.Low == nil || between.High == nil {
		t.Fatalf("a negated BETWEEN keeps both bounds and its negation: %#v", between)
	}

	is, ok := parseOne(t, `title IS NOT NULL`).(*Is)
	if !ok || !is.Negated {
		t.Fatalf("IS NOT keeps its negation: %#v", is)
	}

	caseExpr, ok := parseOne(t, `CASE WHEN score > 1 THEN 'a' ELSE 'b' END = 'a'`).(*Binary)
	if !ok {
		t.Fatal("expected a comparison at the root")
	}
	branch, ok := caseExpr.Left.(*Case)
	if !ok || len(branch.Branches) != 1 || branch.Else == nil {
		t.Fatalf("a CASE keeps its branches and its else: %#v", caseExpr.Left)
	}
}

func TestATreeIsNotBuiltForARejectedExpression(t *testing.T) {
	if node, err := Parse(`md5(title) = ?`, Options{Columns: tableColumns, Args: []any{"x"}}); err == nil || node != nil {
		t.Fatalf("a rejected filter must yield no tree: %#v %v", node, err)
	}
}
