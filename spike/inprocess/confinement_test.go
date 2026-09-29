package inprocess

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seed(t *testing.T, opts Options) *Namespace {
	t.Helper()
	ns := NewNamespace("app", opts)
	err := ns.CreateTable(TableSpec{
		Name: "notes",
		Columns: []Column{
			{Name: "id", Type: Int64()},
			{Name: "body", Type: Text()},
			{Name: "score", Type: Float64()},
			{Name: "flag", Type: Bool()},
		},
		Rows: [][]any{
			{int64(1), "first note", 1.5, int8(1)},
			{int64(2), "second note", 2.5, int8(0)},
			{int64(3), "third note", 3.5, int8(1)},
		},
	})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	return ns
}

func TestTheEngineBuildsPureGo(t *testing.T) {
	ns := seed(t, Options{})
	if ns.Engine == nil {
		t.Fatal("no engine")
	}
}

func TestAScanAndAFilterReturnTheRows(t *testing.T) {
	ns := seed(t, Options{})
	rows, err := ns.Query("SELECT id, body FROM notes WHERE id = 2")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("filtered scan returned %d rows, want 1: %v", len(rows), rows)
	}
	if got := rows[0][1]; got != "second note" {
		t.Fatalf("body = %v, want second note", got)
	}
	all, err := ns.Query("SELECT id, body FROM notes ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("unfiltered scan returned %d rows, want 3", len(all))
	}
}

func TestDolmenFieldTypesMapOntoTheEngine(t *testing.T) {
	ns := seed(t, Options{})
	rows, err := ns.Query("SELECT id, score, flag FROM notes ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows", len(rows))
	}
	if _, ok := rows[0][0].(int64); !ok {
		t.Fatalf("id came back as %T, want int64", rows[0][0])
	}
	if _, ok := rows[0][1].(float64); !ok {
		t.Fatalf("score came back as %T, want float64", rows[0][1])
	}
	if rows[0][2] != int8(1) {
		t.Fatalf("flag came back as %T(%v); MySQL stores BOOLEAN as TINYINT, so a dolmen bool field reads as int8 and needs mapping", rows[0][2], rows[0][2])
	}
}

func TestOneEnginePerProcess(t *testing.T) {
	a := NewNamespace("ns_a", Options{})
	b := NewNamespace("ns_b", Options{})
	if a.Engine != b.Engine {
		t.Fatal("two engines were built in one process; go-mysql-server registers global functions and panics on the second, so an engine per namespace is not available")
	}
	if a.DB == b.DB {
		t.Fatal("two namespaces share a database")
	}
}

func TestCrossNamespaceReadsAreNotConfined(t *testing.T) {
	useConfinedEngine(t)
	ns := seed(t, Options{ReadOnly: true, Locked: true})
	_, err := ns.WithOther("otherdb", func(o *Namespace) error {
		return o.CreateTable(TableSpec{
			Name:    "secrets",
			Columns: []Column{{Name: "id", Type: Int64()}, {Name: "v", Type: Text()}},
			Rows:    [][]any{{int64(1), "other-namespace-secret"}},
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := ns.Query("SELECT v FROM otherdb.secrets")
	if err == nil && len(rows) == 1 && rows[0][0] == "other-namespace-secret" {
		t.Logf("a read of another namespace's table succeeded and returned its contents: confinement by registration does not hold in-process")
	} else {
		t.Logf("cross-namespace read refused: err=%v rows=%v", err, rows)
	}
	for _, q := range []string{
		"SELECT * FROM information_schema.schemata",
		"SHOW DATABASES",
	} {
		r, qerr := ns.Query(q)
		if qerr == nil && len(r) > 1 {
			t.Logf("%q enumerated %d databases, so a caller can discover other namespaces by name: %v", q, len(r), r)
		}
	}
}

func TestWritesAndDDLAreRefused(t *testing.T) {
	useConfinedEngine(t)
	ns := seed(t, Options{ReadOnly: true, Locked: true})
	for _, q := range []string{
		"CREATE TABLE evil (a INT)",
		"INSERT INTO notes (id, body) VALUES (9, 'x')",
		"UPDATE notes SET body = 'x'",
		"DELETE FROM notes",
		"DROP TABLE notes",
		"LOAD DATA INFILE '/etc/hosts' INTO TABLE notes",
	} {
		if _, err := ns.Query(q); err == nil {
			t.Errorf("%q was allowed, want a refusal", q)
		}
	}
}

func TestTheFileEscapesAreOpenAndUnclosable(t *testing.T) {
	useConfinedEngine(t)
	ns := seed(t, Options{ReadOnly: true, Locked: true})
	root := t.TempDir()
	secret := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(secret, []byte("top-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	outfile := filepath.Join(root, "escaped.txt")

	rows, err := ns.Query("SELECT LOAD_FILE('" + secret + "')")
	if err != nil {
		t.Fatalf("LOAD_FILE returned an error, which is the outcome this spike did not find: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("LOAD_FILE returned %d rows, want 1", len(rows))
	}
	got, ok := rows[0][0].([]byte)
	if !ok || string(got) != "top-secret" {
		t.Fatalf("LOAD_FILE returned %T(%v), want the file's bytes: the confinement did not hold", rows[0][0], rows[0][0])
	}

	if _, err := ns.Query("SELECT body FROM notes INTO OUTFILE '" + outfile + "'"); err != nil {
		t.Fatalf("INTO OUTFILE returned an error: %v", err)
	}
	written, readErr := os.ReadFile(outfile)
	if readErr != nil {
		t.Fatalf("INTO OUTFILE did not write: %v", readErr)
	}
	if !strings.Contains(string(written), "first note") {
		t.Fatalf("INTO OUTFILE wrote %q, want the table's rows", string(written))
	}

	t.Logf("LOAD_FILE read %d bytes from a file outside the namespace", len(got))
	t.Logf("INTO OUTFILE wrote %d bytes to %s", len(written), outfile)
}

func TestNoConfigurationClosesTheFileEscapes(t *testing.T) {
	useConfinedEngine(t)
	ns := seed(t, Options{ReadOnly: true, Locked: true})
	for _, q := range []string{
		"SET GLOBAL secure_file_priv = '/nowhere'",
		"SET GLOBAL local_infile = 0",
		"SET @@secure_file_priv = ''",
	} {
		_, err := ns.Query(q)
		t.Logf("%-40s err=%v", q, err)
	}
	secret := filepath.Join(t.TempDir(), "s.txt")
	if err := os.WriteFile(secret, []byte("still-readable"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows, err := ns.Query("SELECT LOAD_FILE('" + secret + "')")
	if err != nil || len(rows) != 1 {
		t.Logf("a SET closed the file escape after all: err=%v rows=%v — re-evaluate the finding, a confined engine is now possible", err, rows)
		return
	}
	t.Log("no SET closes the file escape, and Q4 rules out a statement filter in dolmen")
}
