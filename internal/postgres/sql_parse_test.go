package postgres

import (
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func queryTestTables() map[string]tableState {
	return map[string]tableState{"notes": {physical: "notes_physical", schema: &schema.TableSchema{Fields: []schema.Field{{Name: "body"}, {Name: "n", Type: schema.Number}}}, columns: map[string]string{"body": "body_physical", "n": "n"}}, "other": {physical: "other", schema: &schema.TableSchema{Fields: []schema.Field{{Name: "body"}}}, columns: map[string]string{"body": "body"}}}
}

func TestPostgresSQLCompiler(t *testing.T) {
	for _, test := range []struct {
		sql  string
		args int
	}{
		{"SELECT * FROM notes WHERE n > ? ORDER BY id", 1},
		{"SELECT n.body, count(*) OVER () AS total FROM notes n LEFT JOIN other o ON n.body=o.body GROUP BY n.body", 0},
		{"WITH q AS (SELECT * FROM notes) SELECT q.body FROM q", 0},
		{"WITH RECURSIVE q(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM q WHERE n < 3) SELECT * FROM q", 0},
		{"SELECT body FROM notes WHERE EXISTS(SELECT 1 FROM other WHERE other.body=notes.body)", 0},
		{"SELECT CASE WHEN n > 1 THEN upper(body) ELSE 'none' END FROM notes", 0},
		{"SELECT CAST(? AS numeric), CURRENT_TIMESTAMP, coalesce(?, 'fallback')", 2},
		{"SELECT '{\"key\":1}'::jsonb ?? 'key'", 0},
		{"SELECT x FROM generate_series(1,3) AS x", 0},
	} {
		out, _, err := compileSQL(test.sql, test.args, "namespace_schema", queryTestTables())
		if err != nil {
			t.Errorf("%s: %v", test.sql, err)
		} else if strings.Contains(out, "FROM notes ") {
			t.Errorf("table not rewritten: %s", out)
		}
	}
}

func TestPostgresSQLBoundaryRejectsEscapes(t *testing.T) {
	for _, sql := range []string{
		"SELECT * FROM pg_catalog.pg_class", "SELECT * FROM information_schema.tables", "SELECT * FROM other_namespace.notes", "SELECT * FROM pg_class",
		"SELECT current_user", "SELECT session_user", "SELECT current_database()", "SELECT current_setting('data_directory')", "SELECT pg_read_file('/etc/passwd')", "SELECT pg_sleep(10)", "SELECT set_config('role','postgres',false)", "SELECT public.lower('x')", "SELECT 'pg_class'::regclass", "SELECT 1; DELETE FROM notes", "WITH gone AS (DELETE FROM notes RETURNING *) SELECT * FROM gone", "SELECT * INTO copied FROM notes", "SELECT * FROM notes FOR UPDATE", "COPY notes TO STDOUT", "SELECT 1 OPERATOR(public.+) 2", "SELECT * FROM notes ORDER BY n USING OPERATOR(public.<)", "WITH q AS (SELECT * FROM pg_class), pg_class AS (SELECT * FROM notes) SELECT * FROM q",
	} {
		if out, _, err := compileSQL(sql, 0, "namespace_schema", queryTestTables()); err == nil {
			t.Errorf("accepted %s as %s", sql, out)
		}
	}
}

func TestPostgresSQLLongNamesAndPlaceholders(t *testing.T) {
	long := strings.Repeat("a", 64)
	tables := queryTestTables()
	tables[long] = tableState{physical: "short_table", schema: &schema.TableSchema{Fields: []schema.Field{{Name: long}}}, columns: map[string]string{long: "short_field"}}
	out, names, err := compileSQL("SELECT "+ident(long)+" FROM "+ident(long)+" WHERE "+ident(long)+"=?", 1, "namespace_schema", tables)
	if err != nil || !strings.Contains(out, "short_table") || !strings.Contains(out, "short_field") || names.original(names.name(long)) != long {
		t.Fatalf("long mapping: %s %v", out, err)
	}
	source := `SELECT '?' AS "?", $$ ? $$, $tag$ ? $tag$, E'it\'s ?', ? /* ? /* ? */ ? */ -- ?
, ?`
	rewritten, count, err := rewriteSQL(source, newSQLNames(nil))
	if err != nil || count != 2 || !strings.Contains(rewritten, "$1 /*") || !strings.HasSuffix(rewritten, "$2") {
		t.Fatalf("lexical parameters: %s %d %v", rewritten, count, err)
	}
	for _, invalid := range []string{"SELECT 'unfinished", "SELECT /* unfinished", "SELECT $tag$ unfinished", "SELECT $1"} {
		if _, _, err := rewriteSQL(invalid, newSQLNames(nil)); err == nil {
			t.Fatalf("accepted malformed SQL: %s", invalid)
		}
	}
}
