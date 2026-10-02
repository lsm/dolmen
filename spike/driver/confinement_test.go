package driver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type attack struct {
	Name string
	SQL  string
}

func attacksFor(root string) []attack {
	secret := filepath.Join(root, "secret.txt")
	other := filepath.Join(root, "other", "events")
	return []attack{
		{"read_parquet_outside_the_namespace", "SELECT * FROM read_parquet('" + secret + "')"},
		{"read_csv_outside_the_namespace", "SELECT * FROM read_csv('" + secret + "')"},
		{"read_text_outside_the_namespace", "SELECT * FROM read_text('" + secret + "')"},
		{"glob_outside_the_namespace", "SELECT * FROM glob('" + filepath.Join(root, "*") + "')"},
		{"glob_the_whole_data_dir", "SELECT * FROM glob('" + root + "/**/*')"},
		{"a_url_table", "SELECT * FROM read_parquet('https://example.invalid/x.parquet')"},
		{"an_s3_url_table", "SELECT * FROM read_parquet('s3://bucket/x.parquet')"},
		{"attach_another_database", "ATTACH '" + root + "/other.db' AS other"},
		{"copy_rows_out_to_a_file", "COPY (SELECT * FROM events) TO '" + filepath.Join(root, "leak.csv") + "'"},
		{"copy_rows_out_to_stdout", "COPY (SELECT * FROM events) TO STDOUT"},
		{"install_an_extension", "INSTALL httpfs"},
		{"load_an_extension", "LOAD httpfs"},
		{"read_the_environment", "SELECT getenv('HOME')"},
		{"read_another_namespaces_directory", "SELECT * FROM read_parquet('" + filepath.Join(other, "x.parquet") + "')"},
		{"create_an_external_table", "CREATE EXTERNAL TABLE stolen (a INT) LOCATION '" + root + "'"},
		{"set_a_setting_after_the_lock", "SET memory_limit='1GB'"},
		{"reset_a_setting_after_the_lock", "RESET memory_limit"},
		{"a_pragma", "PRAGMA version"},
		{"create_a_table", "CREATE TABLE t (a INT)"},
		{"drop_a_table", "DROP TABLE events"},
		{"insert_rows", "INSERT INTO events VALUES (1,'x',1.0,'y',true)"},
		{"update_rows", "UPDATE events SET body='x'"},
		{"delete_rows", "DELETE FROM events"},
		{"two_statements_at_once", "SELECT 1; DROP TABLE events"},
	}
}

func plantSecrets(t *testing.T, root string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("TOP-SECRET-VALUE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "other", "events")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "x.parquet"), []byte("not-a-parquet-file"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEveryEscapeIsRefusedUnderTheLockAndSaysWhichMechanismRefusedIt(t *testing.T) {
	tbl := newFixture(t, 20)
	_, lockedSidecar, root := locked(t, tbl)
	plantSecrets(t, root)

	for _, a := range attacksFor(root) {
		t.Run(a.Name, func(t *testing.T) {
			res, err := queryWithin(t, lockedSidecar, a.SQL, attackBudget)
			if err == nil {
				t.Fatalf("%s was accepted under the lock: %+v", a.SQL, res.Rows)
			}
			msg := err.Error()
			for _, leak := range []string{"TOP-SECRET-VALUE", "leak.csv"} {
				if strings.Contains(msg, leak) {
					t.Fatalf("the refusal message carried %q out of the namespace: %s", leak, msg)
				}
			}
			if got := errorClass(err); got == "transport" {
				t.Fatalf("the sidecar broke instead of refusing %q: %v", a.SQL, err)
			}
			t.Logf("%s refused with class=%s: %s", a.Name, errorClass(err), msg)
		})
	}
}

func TestTheSidecarStillServesLegitimateQueriesAfterEveryEscapeWasRefused(t *testing.T) {
	tbl := newFixture(t, 20)
	_, lockedSidecar, root := locked(t, tbl)
	plantSecrets(t, root)

	for _, a := range attacksFor(root) {
		_, _ = queryWithin(t, lockedSidecar, a.SQL, attackBudget)
	}
	res := mustQuery(t, lockedSidecar, "SELECT count(*) FROM events")
	if res.Rows == nil || res.Rows[0] == "" {
		t.Fatal("the sidecar stopped answering legitimate queries after the escape attempts")
	}
	if _, err := query(t, lockedSidecar, "SELECT * FROM read_parquet('"+filepath.Join(root, "secret.txt")+"')"); err == nil {
		t.Fatal("the lock was lost after the escape attempts")
	}
}

func TestWhichRefusalsAreTheMechanismAndWhichAreOnlyDefaultsThatCouldBeFlipped(t *testing.T) {
	tbl := newFixture(t, 20)
	_, lockedSidecar, root := locked(t, tbl)
	plantSecrets(t, root)

	_, unlockedSidecar, _ := unlocked(t, tbl)

	var mechanism, defaults []string
	for _, a := range attacksFor(root) {
		_, lockedErr := queryWithin(t, lockedSidecar, a.SQL, attackBudget)
		_, unlockedErr := queryWithin(t, unlockedSidecar, a.SQL, attackBudget)
		switch {
		case lockedErr == nil:
			t.Errorf("%s SUCCEEDED under the lock, so the confinement does not hold", a.Name)
		case unlockedErr == nil:
			mechanism = append(mechanism, a.Name)
			t.Logf("mechanism: %s is refused only because of the lock", a.Name)
		default:
			defaults = append(defaults, a.Name)
			t.Logf("default: %s is refused with the lock off too, so it rests on an engine default", a.Name)
		}
	}
	t.Logf("mechanism (%d): %v", len(mechanism), mechanism)
	t.Logf("defaults (%d): %v", len(defaults), defaults)
}

func TestAnotherNamespacesRowsAreNotReachableThroughTheRegisteredTable(t *testing.T) {
	tbl := newFixture(t, 20)
	_, sc, root := locked(t, tbl)
	plantSecrets(t, root)

	other := filepath.Join(root, "other", "events")
	for _, sql := range []string{
		"SELECT * FROM read_parquet('" + filepath.Join(other, "*.parquet") + "')",
		"SELECT * FROM '" + filepath.Join(other, "x.parquet") + "'",
		"ATTACH '" + other + "' (TYPE ICEBERG)",
	} {
		if _, err := queryWithin(t, sc, sql, attackBudget); err == nil {
			t.Errorf("%q reached into another namespace's directory", sql)
		}
	}
}

func TestTheSecretFileOutsideTheNamespaceNeverAppearsInAnyResult(t *testing.T) {
	tbl := newFixture(t, 20)
	_, sc, root := locked(t, tbl)
	plantSecrets(t, root)

	for _, sql := range []string{
		"SELECT * FROM events",
		"SELECT * FROM read_parquet('" + filepath.Join(root, "**", "*.parquet") + "')",
		"SELECT * FROM information_schema.tables",
	} {
		res, err := queryWithin(t, sc, sql, attackBudget)
		if err != nil {
			continue
		}
		for _, row := range res.Rows {
			if strings.Contains(row, "TOP-SECRET-VALUE") {
				t.Fatalf("%q returned the secret: %s", sql, row)
			}
		}
	}
}
