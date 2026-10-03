package driver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lsm/dolmen/spike/driver/fixture"
)

type attack struct {
	Name string
	SQL  string
}

func attacksFor(tbl *fixture.Table) []attack {
	root := rootOf(tbl)
	outside := tbl.Outside
	other := filepath.Join(filepath.Dir(root), "other", fixture.TableName)
	text := filepath.Join(outside, "secret.txt")
	csv := filepath.Join(outside, "secret.csv")
	parquet := filepath.Join(outside, "secret.parquet")
	return []attack{
		{"read_parquet_outside_the_namespace", "SELECT * FROM read_parquet('" + parquet + "')"},
		{"read_csv_outside_the_namespace", "SELECT * FROM read_csv('" + csv + "')"},
		{"read_text_outside_the_namespace", "SELECT * FROM read_text('" + text + "')"},
		{"glob_outside_the_namespace", "SELECT * FROM glob('" + filepath.Join(outside, "*") + "')"},
		{"glob_the_whole_data_dir", "SELECT * FROM glob('" + outside + "/**/*')"},
		{"a_url_table", "SELECT * FROM read_parquet('https://example.invalid/x.parquet')"},
		{"an_s3_url_table", "SELECT * FROM read_parquet('s3://bucket/x.parquet')"},
		{"attach_another_database", "ATTACH '" + filepath.Join(outside, "fresh.db") + "' AS other"},
		{"copy_rows_out_to_a_file", "COPY (SELECT * FROM events) TO '" + filepath.Join(outside, "leak.csv") + "'"},
		{"copy_rows_out_to_stdout", "COPY (SELECT * FROM events) TO STDOUT"},
		{"install_an_extension", "INSTALL httpfs"},
		{"load_an_extension", "LOAD httpfs"},
		{"read_the_environment", "SELECT getenv('HOME')"},
		{"read_another_namespaces_directory", "SELECT * FROM read_parquet('" + filepath.Join(other, "x.parquet") + "')"},
		{"create_an_external_table", "CREATE EXTERNAL TABLE stolen (a INT) LOCATION '" + outside + "'"},
		{"set_a_setting_after_the_lock", "SET memory_limit='1GB'"},
		{"reset_a_setting_after_the_lock", "RESET memory_limit"},
		{"a_pragma", "PRAGMA memory_limit='64GB'"},
		{"create_a_table", "CREATE TABLE t (a INT)"},
		{"drop_a_table", "DROP TABLE events"},
		{"insert_rows", "INSERT INTO events VALUES (1,'x',1.0,'y',true)"},
		{"update_rows", "UPDATE events SET body='x'"},
		{"delete_rows", "DELETE FROM events"},
		{"two_statements_at_once", "SELECT 1; DROP TABLE events"},
	}
}

func plantEverything(t *testing.T, tbl *fixture.Table) {
	t.Helper()
	if err := fixture.PlantSecretOutside(rootOf(tbl)); err != nil {
		t.Fatalf("plant the secret outside the namespace: %v", err)
	}
	if _, err := fixture.PlantOtherNamespace(rootOf(tbl)); err != nil {
		t.Fatalf("plant a second namespace: %v", err)
	}
}

func TestEveryEscapeIsRefusedUnderTheLock(t *testing.T) {
	tbl := newFixture(t, 20)
	_, sc, _ := locked(t, tbl)
	plantEverything(t, tbl)

	for _, a := range attacksFor(tbl) {
		t.Run(a.Name, func(t *testing.T) {
			res, err := queryWithin(t, sc, a.SQL, attackBudget)
			if err == nil {
				t.Fatalf("%s was accepted under the lock, returning %q", a.SQL, res.Rows)
			}
			if got := errorClass(err); got == "transport" {
				t.Fatalf("the sidecar broke instead of refusing %q: %v", a.SQL, err)
			}
			t.Logf("%s refused with class=%s: %s", a.Name, errorClass(err), err.Error())
		})
	}
}

func TestTheSecretOutsideTheNamespaceNeverComesBackInAnyResult(t *testing.T) {
	tbl := newFixture(t, 20)
	_, sc, _ := locked(t, tbl)
	plantEverything(t, tbl)

	for _, a := range attacksFor(tbl) {
		res, err := queryWithin(t, sc, a.SQL, attackBudget)
		if err != nil {
			continue
		}
		for _, row := range res.Rows {
			if strings.Contains(row, "TOP-SECRET-VALUE") {
				t.Fatalf("%q returned the secret from outside the namespace: %s", a.SQL, row)
			}
		}
	}
}

func TestTheSidecarStillServesLegitimateQueriesAfterEveryEscapeWasRefused(t *testing.T) {
	tbl := newFixture(t, 20)
	_, sc, _ := locked(t, tbl)
	plantEverything(t, tbl)

	for _, a := range attacksFor(tbl) {
		_, _ = queryWithin(t, sc, a.SQL, attackBudget)
	}
	res := mustQuery(t, sc, "SELECT count(*) FROM events")
	if res.Rows == nil || res.Rows[0] == "" {
		t.Fatal("the sidecar stopped answering legitimate queries after the escape attempts")
	}
	if _, err := queryWithin(t, sc, "SELECT * FROM read_text('"+filepath.Join(tbl.Outside, "secret.txt")+"')", attackBudget); err == nil {
		t.Fatal("the lock was lost after the escape attempts")
	}
}

func TestWhichRefusalsAreTheMechanismAndWhichAreOnlyDefaultsThatCouldBeFlipped(t *testing.T) {
	tbl := newFixture(t, 20)
	_, lockedSidecar, _ := locked(t, tbl)
	plantEverything(t, tbl)
	_, unlockedSidecar, _ := unlocked(t, tbl)

	var mechanism, defaults []string
	for _, a := range attacksFor(tbl) {
		lockedRes, lockedErr := queryWithin(t, lockedSidecar, a.SQL, attackBudget)
		_, unlockedErr := queryWithin(t, unlockedSidecar, a.SQL, attackBudget)
		if lockedErr == nil {
			for _, row := range lockedRes.Rows {
				if strings.Contains(row, "TOP-SECRET-VALUE") {
					t.Errorf("%s LEAKED the secret: %s", a.Name, row)
				}
			}
			t.Errorf("%s SUCCEEDED under the lock, so the confinement does not hold", a.Name)
			continue
		}
		if unlockedErr == nil {
			mechanism = append(mechanism, a.Name)
			t.Logf("mechanism: %s is refused only because of the lock", a.Name)
			continue
		}
		defaults = append(defaults, a.Name)
		t.Logf("default: %s is refused with the lock off too, so it rests on an engine default", a.Name)
	}
	t.Logf("SUMMARY mechanism=%d default=%d", len(mechanism), len(defaults))
	t.Logf("SUMMARY mechanism_list=%v", mechanism)
	t.Logf("SUMMARY default_list=%v", defaults)
}

func TestTheUnlockedSidecarCanActuallyReadTheSecretSoTheComparisonIsReal(t *testing.T) {
	tbl := newFixture(t, 20)
	_, unlockedSidecar, _ := unlocked(t, tbl)
	plantEverything(t, tbl)

	sql := "SELECT * FROM read_text('" + filepath.Join(tbl.Outside, "secret.txt") + "')"
	res, err := queryWithin(t, unlockedSidecar, sql, attackBudget)
	if err != nil {
		t.Skipf("the unlocked sidecar could not read the secret either, so this run cannot separate mechanism from default: %v", err)
	}
	found := false
	for _, row := range res.Rows {
		if strings.Contains(row, "TOP-SECRET-VALUE") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the unlocked sidecar read the file but returned %q, so the control does not prove the secret was reachable", res.Rows)
	}
}

func TestNothingTheSidecarWroteEscapedIntoTheDirectoryOutsideTheNamespace(t *testing.T) {
	tbl := newFixture(t, 20)
	_, sc, _ := locked(t, tbl)
	plantEverything(t, tbl)

	for _, a := range attacksFor(tbl) {
		_, _ = queryWithin(t, sc, a.SQL, attackBudget)
	}
	entries, err := os.ReadDir(tbl.Outside)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		switch e.Name() {
		case "secret.txt", "secret.csv", "secret.parquet":
		default:
			t.Errorf("the sidecar created %s outside the namespace", e.Name())
		}
	}
}
