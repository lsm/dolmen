package duckdblockdown

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func duckdbBinary(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("DOLMEN_TEST_DUCKDB")
	if bin == "" {
		t.Skip("set DOLMEN_TEST_DUCKDB to a duckdb CLI binary to run the lockdown tests")
	}
	info, err := os.Stat(bin)
	if err != nil || info.IsDir() {
		t.Fatalf("DOLMEN_TEST_DUCKDB=%q is not a file: %v", bin, err)
	}
	return bin
}

type fixture struct {
	locked  Locked
	nsDir   string
	sibling string
	canary  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	bin := duckdbBinary(t)
	root := t.TempDir()
	ns := filepath.Join(root, "ns")
	sibling := filepath.Join(root, "sibling")
	if err := os.MkdirAll(LockdownDir(ns), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(LockdownDir(sibling), 0o700); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(LockdownDir(ns), "rows.csv")
	if err := os.WriteFile(own, []byte("id,v\n1,hello\n2,world\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(LockdownDir(sibling), "canary.csv")
	if err := os.WriteFile(canary, []byte("id,v\n9,secret-sibling-namespace\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog := filepath.Join(ns, "catalog.db")
	if err := os.WriteFile(catalog, []byte("id,v\n7,sqlite-catalog\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(LockdownDir(ns), "escape")
	if err := os.Symlink(LockdownDir(sibling), inside); err != nil {
		t.Skipf("this platform does not allow the symlink fixture: %v", err)
	}
	locked, err := Lock(Options{Bin: bin, Namespace: ns, MemoryLimit: "512MiB", Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { locked.Close() })
	return &fixture{locked: locked, nsDir: ns, sibling: sibling, canary: canary}
}

func (f *fixture) ctx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestTheTraversalCaseSendsAnUnresolvedTraversalToDuckDB(t *testing.T) {
	f := newFixture(t)
	literal := f.locked.DataDir + "/../../sibling/data/canary.csv"
	if !strings.Contains(literal, "/../") {
		t.Fatalf("the traversal fixture resolved itself away: %q", literal)
	}
	cleaned := filepath.Clean(literal)
	if cleaned == literal {
		t.Fatalf("the traversal fixture is already clean, so it is not a traversal: %q", literal)
	}
	if !strings.HasPrefix(cleaned, filepath.Dir(filepath.Dir(LockdownDir(f.sibling)))+string(filepath.Separator)) {
		t.Fatalf("the traversal fixture should resolve into the sibling namespace, got %q from %q", cleaned, literal)
	}
}

func TestTheLockedSettingsActuallyTakeEffect(t *testing.T) {
	f := newFixture(t)
	res, err := f.locked.Run(f.ctx(t), f.locked.settingsQuery())
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("reading back settings: %d %s", res.ExitCode, res.Combined())
	}
	out := res.Stdout
	if !strings.Contains(out, "false") {
		t.Fatalf("enable_external_access is not false in %q; the lock did not take effect", out)
	}
	if err := f.locked.VerifySettings(f.ctx(t)); err != nil {
		t.Fatal(err)
	}
}

func TestTheLockedEngineStillReadsItsOwnNamespace(t *testing.T) {
	f := newFixture(t)
	own := filepath.Join(f.locked.DataDir, "rows.csv")
	res, err := f.locked.Run(f.ctx(t), "SELECT count(*) AS n FROM read_csv_auto('"+own+"');")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("the engine must read its own namespace: %d %s", res.ExitCode, res.Combined())
	}
	if !strings.Contains(res.Stdout, "2") {
		t.Fatalf("own-file read returned %q, want the two seeded rows", res.Stdout)
	}
}

func TestConfinementBlocksEveryFilesystemEscape(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx(t)
	siblingData := LockdownDir(f.sibling)
	ownEscape := filepath.Join(f.locked.DataDir, "escape")
	catalog := filepath.Join(f.nsDir, "catalog.db")
	cases := []struct {
		name string
		sql  string
	}{
		{"attach a sibling namespace database", "ATTACH '" + filepath.Join(siblingData, "sibling.db") + "' AS evil;"},
		{"read a sibling namespace file", "SELECT * FROM read_csv_auto('" + f.canary + "');"},
		{"read the namespace's own sqlite catalog", "SELECT * FROM read_csv_auto('" + catalog + "');"},
		{"read through a symlink planted inside the allowed directory", "SELECT * FROM read_csv_auto('" + filepath.Join(ownEscape, "canary.csv") + "');"},
		{"traverse out with ..", "SELECT * FROM read_csv_auto('" + f.locked.DataDir + "/../../sibling/data/canary.csv');"},
		{"copy to outside the directory", "COPY (SELECT 1 AS a) TO '" + filepath.Join(siblingData, "pwned.csv") + "';"},
		{"copy from outside the directory", "CREATE TABLE t(a INT); COPY t FROM '" + f.canary + "';"},
		{"copy to a shell program", "COPY (SELECT 1) TO PROGRAM 'id > " + filepath.Join(siblingData, "pwned.txt") + "';"},
		{"read parquet outside", "SELECT * FROM read_parquet('" + filepath.Join(siblingData, "*.parquet") + "');"},
		{"read text outside", "SELECT * FROM read_text('" + f.canary + "');"},
		{"glob outside", "SELECT * FROM glob('" + filepath.Join(siblingData, "*") + "');"},
		{"read an http url", "SELECT * FROM read_csv_auto('https://example.com/x.csv');"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := f.locked.Run(ctx, c.sql)
			if err != nil {
				t.Fatal(err)
			}
			if res.ExitCode == 0 {
				t.Fatalf("confinement did not block %q: exit 0, output %q", c.name, res.Combined())
			}
			if res.ContainsAny("secret-sibling-namespace", "sqlite-catalog") {
				t.Fatalf("%q returned data it must not reach: %q", c.name, res.Combined())
			}
		})
	}
	t.Run("nothing was written outside", func(t *testing.T) {
		entries, err := os.ReadDir(siblingData)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Name() != "canary.csv" {
				t.Fatalf("a confined write landed in the sibling namespace: %s", e.Name())
			}
		}
	})
}

func TestConfinementBlocksExtensionInstallAndLoad(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx(t)
	for _, c := range []struct{ name, sql string }{
		{"install an extension", "INSTALL httpfs;"},
		{"load an extension", "LOAD httpfs;"},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, err := f.locked.Run(ctx, c.sql)
			if err != nil {
				t.Fatal(err)
			}
			if res.ExitCode == 0 {
				t.Fatalf("confinement did not block %q: exit 0, output %q", c.name, res.Combined())
			}
		})
	}
}

func TestConfinementBlocksReopeningTheSettings(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx(t)
	for _, c := range []struct{ name, sql string }{
		{"set enable_external_access", "SET enable_external_access = true;"},
		{"set allowed_directories to root", "SET allowed_directories = ['/'];"},
		{"reset lock_configuration", "RESET lock_configuration;"},
		{"set a variable that shadows a setting", "SET VARIABLE allowed_directories = ['/']; SELECT getvariable('allowed_directories');"},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, err := f.locked.Run(ctx, c.sql)
			if err != nil {
				t.Fatal(err)
			}
			if c.name == "set a variable that shadows a setting" {
				t.Skip("a session variable is not the setting; the read-back below is the assertion")
			}
			if res.ExitCode == 0 {
				t.Fatalf("confinement did not block %q: exit 0, output %q", c.name, res.Combined())
			}
		})
	}
	t.Run("the settings are still locked afterwards", func(t *testing.T) {
		if err := f.locked.VerifySettings(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

func TestTheSessionVariableDoesNotWidenTheSetting(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx(t)
	res, err := f.locked.Run(ctx, "SET VARIABLE allowed_directories = ['/']; SELECT current_setting('allowed_directories') AS dirs;")
	if err != nil {
		t.Fatal(err)
	}
	dirs := firstDataCell(res.Stdout)
	if dirs == "" {
		t.Fatalf("the setting did not read back in the same session: %q", res.Stdout)
	}
	if !strings.Contains(dirs, "ns/data") {
		t.Fatalf("the session variable changed the setting's own value to %q, want it still naming the namespace data directory", dirs)
	}
	if strings.TrimSpace(dirs) == "[/]" || strings.Contains(dirs, "/],") {
		t.Fatalf("the session variable widened the setting to the filesystem root: %q", dirs)
	}
}

func TestEveryCLIModeRunsADotCommand(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx(t)
	modes := []struct {
		label string
		run   func(sql string) (Result, error)
	}{
		{"stdin", func(sql string) (Result, error) { return f.locked.RunStdin(ctx, sql) }},
		{"stdin-json", func(sql string) (Result, error) { return f.locked.RunStdinMode(ctx, sql, "-json") }},
		{"command-argument", func(sql string) (Result, error) { return f.locked.Run(ctx, sql) }},
	}
	for _, m := range modes {
		t.Run(m.label, func(t *testing.T) {
			marker := filepath.Join(LockdownDir(f.sibling), "dot-"+m.label+".txt")
			defer os.Remove(marker)
			res, err := m.run(".shell touch " + marker)
			if err != nil {
				t.Fatal(err)
			}
			if _, statErr := os.Stat(marker); statErr != nil {
				t.Fatalf("the %s mode refused the dot-command (exit %d, output %q): this test pins the spike's negative result, so a failure here is good news — a mode exists in which the stdio transport may be confinable, and docs/design/lakehouse-plan.md section 2.2 mechanism 1 is worth re-evaluating", m.label, res.ExitCode, res.Combined())
			}
		})
	}
}

func TestCallerSQLCanInjectADotCommandThroughStdin(t *testing.T) {
	f := newFixture(t)
	ctx := f.ctx(t)
	marker := filepath.Join(LockdownDir(f.sibling), "injected.txt")
	defer os.Remove(marker)
	payload := "SELECT 1 AS a;\n.shell touch " + marker + "\n"
	res, err := f.locked.RunStdin(ctx, payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("a dot-command did not reach the CLI from a single statement followed by a newline (exit %d, output %q): this pins the injection the spike measured, so a failure here means the stdio transport may be confinable and docs/design/lakehouse-plan.md section 2.2 mechanism 1 is worth re-evaluating", res.ExitCode, res.Combined())
	}
}

func TestHasDotCommandLineRecognisesTheInjection(t *testing.T) {
	cases := []struct {
		sql  string
		want bool
	}{
		{"SELECT 1;", false},
		{"SELECT 1;\n.shell id", true},
		{"SELECT 1;\n  .system id", true},
		{"SELECT 1;\n.output /tmp/x", true},
		{"SELECT 1;\n.read /tmp/x", true},
		{"SELECT 1;\n", false},
		{"", false},
	}
	for _, c := range cases {
		if got := HasDotCommandLine(c.sql); got != c.want {
			t.Errorf("HasDotCommandLine(%q) = %v, want %v", c.sql, got, c.want)
		}
	}
}

func TestParseSettingRowReadsEachSettingByColumn(t *testing.T) {
	locked := "┌─────────┬─────────┬─────────────┬──────────┬─────────┬──────────────────────┐\n" +
		"│   ext   │  lock   │ autoinstall │ autoload │ secrets │         dirs         │\n" +
		"│ boolean │ boolean │   boolean   │ boolean  │ boolean │      varchar[]       │\n" +
		"├─────────┼─────────┼─────────────┼──────────┼─────────┼──────────────────────┤\n" +
		"│ false   │ true    │ false       │ false    │ false   │ [/tmp/ns/data/]      │\n" +
		"└─────────┴─────────┴─────────────┴──────────┴─────────┴──────────────────────┘\n"
	row, ok := parseSettingRow(locked)
	if !ok {
		t.Fatal("a locked settings row did not parse")
	}
	if row.ExternalAccess != "false" || row.Lock != "true" {
		t.Fatalf("ext=%q lock=%q, want false and true", row.ExternalAccess, row.Lock)
	}
	if !strings.Contains(row.AllowedDirs, "/tmp/ns/data/") {
		t.Fatalf("dirs=%q, want the namespace data directory", row.AllowedDirs)
	}
	unlocked := strings.Replace(locked, "│ false   │ true    │", "│ true    │ true    │", 1)
	row, ok = parseSettingRow(unlocked)
	if !ok {
		t.Fatal("an unlocked settings row did not parse")
	}
	if row.ExternalAccess != "true" {
		t.Fatalf("ext=%q, want true: the parser must not confuse this column with the four false ones", row.ExternalAccess)
	}
	if _, ok := parseSettingRow("no table here\n"); ok {
		t.Fatal("nonsense parsed as a settings row")
	}
}
