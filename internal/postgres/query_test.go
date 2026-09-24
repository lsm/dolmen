package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
	"github.com/lsm/dolmen/skill"
)

func TestPostgresQueryNamespaceBoundary(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	for _, ns := range []string{"app", "foreign"} {
		if err := s.CreateNamespace(ctx, ns, [16]byte{}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateTable(ctx, ns, "notes", []schema.Field{{Name: "body", Vectorize: true}, {Name: "n", Type: schema.Number}, {Name: "metadata", Type: schema.JSON}}, store.TableOpts{}, [16]byte{}); err != nil {
			t.Fatal(err)
		}
		emb := store.Embedder{Identity: "test", Embed: func(context.Context, []string) ([][]float32, error) { return [][]float32{{1, 0}}, nil }}
		if _, err := s.Insert(ctx, ns, "notes", []map[string]any{{"body": ns, "n": int64(9007199254740993), "metadata": map[string]any{"exact": json.Number("9007199254740993")}}}, store.WriteOpts{}, emb, nil, store.Incarnation{}); err != nil {
			t.Fatal(err)
		}
	}
	result, err := s.Query(ctx, "app", "SELECT * FROM notes WHERE n = ?", []any{json.Number("9007199254740993")}, [16]byte{}, store.Page{})
	if err != nil || len(result.Rows) != 1 || result.Rows[0]["body"] != "app" || result.Rows[0]["n"] != int64(9007199254740993) {
		t.Fatalf("query: %+v %v", result, err)
	}
	if _, ok := result.Rows[0]["_embedding"]; ok {
		t.Fatal("hidden embedding exposed")
	}
	if result.Rows[0]["metadata"].(map[string]any)["exact"] != json.Number("9007199254740993") {
		t.Fatalf("JSON changed: %#v", result.Rows)
	}
	err = s.readOnly(ctx, "app", func(tx pgx.Tx, _ namespace) error {
		var readOnly string
		if err := tx.QueryRow(ctx, "SHOW transaction_read_only").Scan(&readOnly); err != nil {
			return err
		}
		if readOnly != "on" {
			return errors.New("read helper opened a writable transaction")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{"SELECT * FROM pg_catalog.pg_class", "SELECT pg_read_file('/etc/passwd')", "SELECT _embedding FROM notes", "WITH x AS (DELETE FROM notes RETURNING *) SELECT * FROM x", "SELECT current_setting('role')"} {
		if _, err := s.Query(ctx, "app", sql, nil, [16]byte{}, store.Page{}); err == nil {
			t.Errorf("unsafe query accepted: %s", sql)
		}
	}
	if _, err := s.Query(ctx, "app", "SELECT 1 AS duplicate, 2 AS duplicate", nil, [16]byte{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("duplicate label: %v", err)
	}
	if _, err := s.Query(ctx, "app", "SELECT 'NaN'::float4", nil, [16]byte{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("nonfinite result: %v", err)
	}
	err = s.read(ctx, "app", func(tx pgx.Tx, n namespace) error {
		if err := enterQueryRole(ctx, tx, s.queryRole); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "SELECT _embedding FROM "+ident(n.physical, "notes"))
		if err == nil {
			return errors.New("query role can read hidden embeddings")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "after_role", []schema.Field{{Name: "body"}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Query(ctx, "app", "SELECT * FROM after_role", nil, [16]byte{}, store.Page{}); err != nil {
		t.Fatalf("new table not granted: %v", err)
	}
	if _, err := s.Insert(ctx, "app", "after_role", []map[string]any{{"body": "write still works"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatalf("pooled role leaked: %v", err)
	}
}

func TestPostgresQueryLongNamesAndNativeResults(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	long := strings.Repeat("a", 64)
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", long, []schema.Field{{Name: long}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "app", long, []map[string]any{{long: "kept"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "app", "SELECT * FROM "+ident(long), nil, [16]byte{}, store.Page{})
	if err != nil || len(result.Rows) != 1 || result.Rows[0][long] != "kept" {
		t.Fatalf("logical labels: %+v %v", result, err)
	}
	result, err = s.Query(ctx, "app", "WITH q AS (SELECT "+ident(long)+" FROM "+ident(long)+") SELECT "+ident(long)+" FROM q", nil, [16]byte{}, store.Page{})
	if err != nil || result.Rows[0][long] != "kept" {
		t.Fatalf("CTE long names: %+v %v", result, err)
	}
	result, err = s.Query(ctx, "app", `SELECT json_build_object('exact',9007199254740993) AS obj`, nil, [16]byte{}, store.Page{})
	if err != nil || result.Rows[0]["obj"].(map[string]any)["exact"] != json.Number("9007199254740993") {
		t.Fatalf("native JSON: %+v %v", result, err)
	}
	result, err = s.Query(ctx, "app", "SELECT x FROM generate_series(1,5) x ORDER BY x", nil, [16]byte{}, store.Page{Offset: 1, Limit: 2})
	if err != nil || len(result.Rows) != 2 || !result.Truncated || result.Rows[0]["x"] != int64(2) {
		t.Fatalf("pagination: %+v %v", result, err)
	}
	if _, err := s.Query(ctx, "app", "SELECT repeat('x',33*1024*1024) AS body", nil, [16]byte{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("response budget: %v", err)
	}
	if _, err := s.Query(ctx, "app", "SELECT json_build_object('x',repeat('x',33*1024*1024)) AS metadata", nil, [16]byte{}, store.Page{}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("JSON response budget: %v", err)
	}
}

func TestPostgresQueryClearsStaleGrantGeneration(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	old, err := s.ensureQueryRole(ctx, "app", [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DropNamespace(ctx, "app", old); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	s.rememberQueryGrant("app", old)
	if _, err := s.Query(ctx, "app", "SELECT 1 AS n", nil, [16]byte{}, store.Page{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale generation: %v", err)
	}
	if _, ok := s.queryGrant("app"); ok {
		t.Fatal("stale grant generation remained cached")
	}
	if _, err := s.Query(ctx, "app", "SELECT 1 AS n", nil, [16]byte{}, store.Page{}); err != nil {
		t.Fatalf("query did not recover after stale grant: %v", err)
	}
}

func TestPostgresQueryRecoversUngrantedTableFromAnotherInstance(t *testing.T) {
	cfg := testConfig(t)
	if cfg.QueryRole == "" {
		t.Skip("set DOLMEN_TEST_PG_QUERY_ROLE for caller SQL grant recovery")
	}
	s := openTest(t, cfg)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body"}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Query(ctx, "app", "SELECT body FROM notes", nil, [16]byte{}, store.Page{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.queryGrant("app"); !ok {
		t.Fatal("grant was not cached")
	}
	ungranted := cfg
	ungranted.QueryRole = ""
	other, err := Open(ctx, ungranted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.CreateTable(ctx, "app", "later", []schema.Field{{Name: "body"}}, store.TableOpts{}, [16]byte{}); err != nil {
		other.Close()
		t.Fatal(err)
	}
	if _, err := other.Insert(ctx, "app", "later", []map[string]any{{"body": "from the other instance"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		other.Close()
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "app", "SELECT body FROM later", nil, [16]byte{}, store.Page{})
	if err != nil {
		t.Fatalf("stale grant cache never recovered: %v", err)
	}
	if len(result.Rows) != 1 || result.Rows[0]["body"] != "from the other instance" {
		t.Fatalf("rows: %+v", result.Rows)
	}
}

func TestPostgresQueryRecoversRevokedGrant(t *testing.T) {
	cfg := testConfig(t)
	if cfg.QueryRole == "" {
		t.Skip("set DOLMEN_TEST_PG_QUERY_ROLE for caller SQL grant recovery")
	}
	s := openTest(t, cfg)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body"}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "kept"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Query(ctx, "app", "SELECT body FROM notes", nil, [16]byte{}, store.Page{}); err != nil {
		t.Fatal(err)
	}
	var physical, table string
	if err := s.read(ctx, "app", func(tx pgx.Tx, n namespace) error {
		state, err := s.loadTable(ctx, tx, n, "notes")
		physical, table = n.physical, state.physical
		return err
	}); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, cfg.DSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, "REVOKE ALL ON "+ident(physical, table)+" FROM "+ident(cfg.QueryRole)); err != nil {
		conn.Close(ctx)
		t.Fatal(err)
	}
	conn.Close(ctx)
	result, err := s.Query(ctx, "app", "SELECT body FROM notes", nil, [16]byte{}, store.Page{})
	if err != nil {
		t.Fatalf("revoked grant never recovered: %v", err)
	}
	if len(result.Rows) != 1 || result.Rows[0]["body"] != "kept" {
		t.Fatalf("rows: %+v", result.Rows)
	}
}

func TestPostgresQueryRendersIntervalAndTimeAsText(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "body"}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "one"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		sql  string
		want string
	}{
		{"SELECT '1 mon 2 day 03:04:05'::interval AS span FROM notes", "1 mon 2 day 03:04:05"},
		{"SELECT '12:34:56'::time AS clock FROM notes", "12:34:56.000000"},
		{"SELECT '12:34:56+02'::timetz AS zoned FROM notes", "12:34:56+02"},
	} {
		result, err := s.Query(ctx, "app", tc.sql, nil, [16]byte{}, store.Page{})
		if err != nil {
			t.Fatalf("%s: %v", tc.sql, err)
		}
		if len(result.Rows) != 1 {
			t.Fatalf("%s: %+v", tc.sql, result.Rows)
		}
		for label, v := range result.Rows[0] {
			got, ok := v.(string)
			if !ok {
				t.Fatalf("%s: column %q decoded to %T (%+v), want a string", tc.sql, label, v, v)
			}
			if got != tc.want {
				t.Fatalf("%s: column %q = %q, want %q", tc.sql, label, got, tc.want)
			}
		}
	}
}

func TestPostgresQueryLoadsTablesInOneRoundTrip(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if _, err := s.CreateTable(ctx, "app", name, []schema.Field{{Name: "body"}, {Name: "n", Type: schema.Number}}, store.TableOpts{}, [16]byte{}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Insert(ctx, "app", name, []map[string]any{{"body": name, "n": 1}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
			t.Fatal(err)
		}
	}
	var loaded map[string]tableState
	if err := s.read(ctx, "app", func(tx pgx.Tx, n namespace) error {
		var err error
		loaded, err = s.queryTables(ctx, tx, n)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 3 {
		t.Fatalf("loaded %d tables", len(loaded))
	}
	for _, name := range []string{"alpha", "beta", "gamma"} {
		state, ok := loaded[name]
		if !ok {
			t.Fatalf("missing %s: %+v", name, loaded)
		}
		if state.schema == nil || state.physical == "" || state.columns["body"] == "" {
			t.Fatalf("%s decoded incompletely: %+v", name, state)
		}
		if state.incarnation.Table != name || state.incarnation.Version != 1 {
			t.Fatalf("%s incarnation: %+v", name, state.incarnation)
		}
	}
	result, err := s.Query(ctx, "app", "SELECT body FROM beta", nil, [16]byte{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 || result.Rows[0]["body"] != "beta" {
		t.Fatalf("query after batch load: %+v", result.Rows)
	}
}

func TestPostgresQueryReservedLongNamePrefixDoesNotCollide(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("v", 64)
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: long}, {Name: "body"}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{long: "encoded", "body": "plain"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "app", `SELECT body AS dolmen_long_0, "`+long+`" FROM notes`, nil, [16]byte{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("rows: %+v", result.Rows)
	}
	row := result.Rows[0]
	if row["dolmen_long_0"] != "plain" {
		t.Fatalf("caller alias using the reserved prefix was remapped: %+v", row)
	}
	if row[long] != "encoded" {
		t.Fatalf("long field lost its logical label: %+v", row)
	}
	if len(row) != 2 {
		t.Fatalf("unexpected labels: %+v", row)
	}
}

func TestPostgresQueryAcceptsBetweenAndOverlaps(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "notes", []schema.Field{{Name: "n", Type: schema.Number}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"n": 1}, {"n": 5}, {"n": 9}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "app", "SELECT n FROM notes WHERE n BETWEEN ? AND ? ORDER BY n", []any{json.Number("2"), json.Number("8")}, [16]byte{}, store.Page{})
	if err != nil {
		t.Fatalf("BETWEEN rejected: %v", err)
	}
	if len(result.Rows) != 1 || result.Rows[0]["n"] != int64(5) {
		t.Fatalf("BETWEEN rows: %+v", result.Rows)
	}
	result, err = s.Query(ctx, "app", "SELECT n FROM notes WHERE n NOT BETWEEN ? AND ? ORDER BY n", []any{json.Number("2"), json.Number("8")}, [16]byte{}, store.Page{})
	if err != nil {
		t.Fatalf("NOT BETWEEN rejected: %v", err)
	}
	if len(result.Rows) != 2 {
		t.Fatalf("NOT BETWEEN rows: %+v", result.Rows)
	}
	if _, err := s.Query(ctx, "app", "SELECT (DATE '2024-01-01', DATE '2024-02-01') OVERLAPS (DATE '2024-01-15', DATE '2024-03-01') AS both FROM notes", nil, [16]byte{}, store.Page{}); err != nil {
		t.Fatalf("OVERLAPS rejected: %v", err)
	}
	if _, err := s.Delete(ctx, "app", "notes", "n BETWEEN ? AND ?", []any{json.Number("2"), json.Number("8")}, store.DeleteOpts{DryRun: true}, nil, store.Incarnation{}); err != nil {
		t.Fatalf("BETWEEN in a mutation filter rejected: %v", err)
	}
}

func TestPostgresFilterErrorsSurviveAnIdentifierNamingTheParseMessage(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "f", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "f", "t", []schema.Field{{Name: "tag"}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Delete(ctx, "f", "t", `"invalid PostgreSQL SQL"(tag) = 1`, nil, store.DeleteOpts{DryRun: true}, nil, store.Incarnation{})
	if err == nil {
		t.Fatal("an unknown function must be rejected")
	}
	if !strings.Contains(err.Error(), "unknown SQL function") {
		t.Fatalf("an identifier that quotes the parse message must keep its own diagnosis, got %v", err)
	}
}

func TestPostgresCallerSQLSeesTheImplicitOwner(t *testing.T) {
	cfg := testConfig(t)
	if cfg.QueryRole == "" {
		t.Skip("set DOLMEN_TEST_PG_QUERY_ROLE to exercise caller SQL")
	}
	s := openTest(t, cfg)
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	fields := []schema.Field{{Name: "body", Type: schema.Text}}
	if _, err := s.CreateTable(ctx, "app", "notes", fields, store.TableOpts{RowAccess: schema.RowAccessOwn}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "app", "notes", []map[string]any{{"body": "x"}}, store.WriteOpts{Owner: "alice"}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	star, err := s.Query(ctx, "app", "SELECT * FROM notes", nil, [16]byte{}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(star.Rows) != 1 {
		t.Fatalf("SELECT * returned %d rows", len(star.Rows))
	}
	if star.Rows[0][schema.OwnerColumn] != "alice" {
		t.Fatalf("a row_access table must show owner in SELECT * the way it shows id and created_at, got %v", star.Rows[0])
	}
	explicit, err := s.Query(ctx, "app", "SELECT owner FROM notes", nil, [16]byte{}, store.Page{})
	if err != nil {
		t.Fatalf("naming owner explicitly must work, which needs the restricted role granted that column: %v", err)
	}
	if len(explicit.Rows) != 1 || explicit.Rows[0][schema.OwnerColumn] != "alice" {
		t.Fatalf("SELECT owner returned %v", explicit.Rows)
	}
}

func TestPostgresQueryAcceptsExtractAndUntypedArguments(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "app", "SELECT extract(year from DATE '2026-03-01') AS y", nil, [16]byte{}, store.Page{})
	if err != nil || len(result.Rows) != 1 {
		t.Fatalf("extract is standard SQL and must be allowed: %+v %v", result, err)
	}
	for _, c := range []struct {
		arg  any
		want any
	}{{5, int64(5)}, {int64(7), int64(7)}, {1.5, 1.5}, {true, int64(1)}, {"x", "x"}} {
		result, err := s.Query(ctx, "app", "SELECT ? AS a", []any{c.arg}, [16]byte{}, store.Page{})
		if err != nil || len(result.Rows) != 1 || result.Rows[0]["a"] != c.want {
			t.Fatalf("SELECT ? AS a with %v: want %v, got %+v %v", c.arg, c.want, result, err)
		}
	}
	result, err = s.Query(ctx, "app", "SELECT x FROM generate_series(1, 5) x WHERE x > ? LIMIT ?", []any{int64(3), int64(1)}, [16]byte{}, store.Page{})
	if err != nil || len(result.Rows) != 1 || result.Rows[0]["x"] != int64(4) {
		t.Fatalf("typed parameters keep their inferred types: %+v %v", result, err)
	}
}

func TestPostgresQueryRunsTheSkillsZonePinnedExample(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "papers", []schema.Field{{Name: "published_at", Type: schema.Timestamp}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "app", "papers", []map[string]any{{"published_at": "2026-01-01T02:00:00Z"}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	expr := skillExample(t, "`(extract\\(year from \\(published_at[^`]*)`")
	result, err := s.Query(ctx, "app", "SELECT "+expr+" AS y FROM papers", nil, [16]byte{}, store.Page{})
	if err != nil || len(result.Rows) != 1 {
		t.Fatalf("the PostgreSQL skill teaches this exact query to pin a zone, so it must run: %+v %v", result, err)
	}
}

func TestPostgresQueryRunsTheSkillsDayBucketingExample(t *testing.T) {
	s := openTest(t, testConfig(t))
	ctx := t.Context()
	if err := s.CreateNamespace(ctx, "app", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTable(ctx, "app", "meetings", []schema.Field{{Name: "started_at", Type: schema.Timestamp}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	recent := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)
	if _, err := s.Insert(ctx, "app", "meetings", []map[string]any{{"started_at": recent}, {"started_at": recent}}, store.WriteOpts{}, store.Embedder{}, nil, store.Incarnation{}); err != nil {
		t.Fatal(err)
	}
	result, err := s.Query(ctx, "app", skillExample(t, "`(SELECT date_trunc[^`]*)`"), nil, [16]byte{}, store.Page{})
	if err != nil || len(result.Rows) != 1 || result.Rows[0]["count"] != int64(2) {
		t.Fatalf("the PostgreSQL skill teaches this exact per-day query, so it must run: %+v %v", result, err)
	}
}

func skillExample(t *testing.T, pattern string) string {
	t.Helper()
	for _, name := range []string{"dolmen", "dolmen-admin"} {
		out, err := skill.Render(name, skill.Context{BaseURL: "http://h", MCPURL: "http://h/mcp", Version: "v", NamespaceHint: skill.DefaultNamespaceHint, Dialect: "postgresql"})
		if err != nil {
			t.Fatal(err)
		}
		if m := regexp.MustCompile(pattern).FindStringSubmatch(string(out)); m != nil {
			return strings.Join(strings.Fields(m[1]), " ")
		}
	}
	t.Fatalf("no example matching %s in the PostgreSQL skills; the test must run what the skill teaches", pattern)
	return ""
}
