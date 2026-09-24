package store

import (
	"strings"
	"testing"
)

func FuzzValidateQueryShape(f *testing.F) {
	for _, s := range []string{"SELECT 1", "WITH x AS (SELECT 1) SELECT * FROM x", "SELECT 1; DROP TABLE t", "/* c */ SELECT 1", "select 1 -- ;\n", "PRAGMA table_info(t)", "SELECT ';'", "SELECT 1 /* ; */", "SELECT 1;;", "SELECT [a;b] FROM t"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, q string) {
		if ValidateQueryShape(q) != nil {
			return
		}
		if !queryStartRe.MatchString(strings.TrimSpace(q)) {
			t.Fatalf("accepted a query that does not begin with SELECT or WITH: %q", q)
		}
		body := stripUnterminatedBlockComment(strings.TrimRight(strings.TrimSpace(q), ";"))
		if hasStatementSeparator(body) {
			t.Fatalf("accepted a query with a second statement: %q", q)
		}
	})
}
