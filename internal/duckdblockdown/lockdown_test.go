package duckdblockdown

import (
	"context"
	"os"
	"os/exec"
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
	seedParquetCanary(t, bin, filepath.Join(LockdownDir(sibling), "canary.parquet"))
	locked, err := Lock(Options{Bin: bin, Namespace: ns, MemoryLimit: "512MiB", Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { locked.Close() })
	return &fixture{locked: locked, nsDir: ns, sibling: sibling, canary: canary}
}

func seedParquetCanary(t *testing.T, bin, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		return
	}
	dir := t.TempDir()
	cmd := exec.Command(bin, "-c", "COPY (SELECT 9::BIGINT AS id, 'secret-sibling-namespace' AS v) TO '"+path+"' (FORMAT PARQUET);")
	cmd.Env = append(os.Environ(), "HOME="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("this duckdb build could not write a parquet canary, so the read_parquet case cannot be measured: %v: %s", err, out)
	}
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
	parquet := filepath.Join(siblingData, "canary.parquet")
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
		{"copy out through a symlink planted inside the allowed directory", "COPY (SELECT 1 AS a) TO '" + filepath.Join(ownEscape, "symlink-write.csv") + "';"},
		{"copy from outside the directory", "CREATE TABLE t(a BIGINT, v VARCHAR); COPY t FROM '" + f.canary + "';"},
		{"copy to a shell program", "COPY (SELECT 1) TO PROGRAM 'id > " + filepath.Join(siblingData, "pwned.txt") + "';"},
		{"read parquet outside", "SELECT * FROM read_parquet('" + parquet + "');"},
		{"read text outside", "SELECT * FROM read_text('" + f.canary + "');"},
		{"glob outside", "SELECT * FROM glob('" + filepath.Join(siblingData, "*") + "');"},
		{"read an http url", "SELECT * FROM read_csv_auto('https://duckdb.org/data/tpch/0_01/parquet/orders.parquet');"},
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
			if !res.ContainsAny("file system operations are disabled", "disabled by configuration", "disabled through configuration") {
				t.Fatalf("%q failed for a reason that is not confinement, so it proves nothing about the sandbox: %q", c.name, res.Combined())
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
		seeded := map[string]bool{"canary.csv": true, "canary.parquet": true}
		for _, e := range entries {
			if !seeded[e.Name()] {
				t.Fatalf("a confined write landed in the sibling namespace: %s", e.Name())
			}
		}
	})
}

func TestAQuoteInThePathCannotAlterTheLock(t *testing.T) {
	bin := duckdbBinary(t)
	root := t.TempDir()
	ns := filepath.Join(root, "it's a namespace")
	if err := os.MkdirAll(LockdownDir(ns), 0o700); err != nil {
		t.Fatal(err)
	}
	locked, err := Lock(Options{Bin: bin, Namespace: ns})
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Close()
	rc, err := os.ReadFile(locked.RCD)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rc), "it''s a namespace") {
		t.Fatalf("the single quote in the data path was not doubled, so path content can terminate the string literal: %s", rc)
	}
	if strings.Contains(string(rc), "it's a namespace") {
		t.Fatalf("an unescaped quote reached the lock file: %s", rc)
	}
	if err := locked.VerifySettings(fctx(t)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(locked.DataDir, "it's a namespace") {
		t.Fatalf("the fixture did not use a path with a quote in it: %q", locked.DataDir)
	}
}

func TestTheEscapeBatteryDetectsAMissingSandbox(t *testing.T) {
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
	canary := filepath.Join(LockdownDir(sibling), "canary.csv")
	if err := os.WriteFile(canary, []byte("id,v\n9,secret-sibling-namespace\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unlocked := Locked{Bin: bin, Home: t.TempDir(), DataDir: LockdownDir(ns)}
	ctx := fctx(t)
	res, err := unlocked.Run(ctx, "SELECT * FROM read_csv_auto('"+canary+"');")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "secret-sibling-namespace") {
		t.Skipf("this duckdb build is already confined without the settings, so the battery cannot be shown to detect a missing sandbox: %q", res.Combined())
	}
	if res.ContainsAny("file system operations are disabled", "disabled by configuration", "disabled through configuration") {
		t.Fatalf("an unconfigured engine already refuses, so the battery's assertion cannot distinguish locked from unlocked: %q", res.Combined())
	}
}

func fctx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
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
	commands := []struct {
		name string
		sql  func(marker string) string
		// oneline is the same command as a single argument, for -c which takes
		// one statement and so cannot carry a follow-up line.
		oneline func(marker string) string
	}{
		{
			name:    "shell",
			sql:     func(marker string) string { return ".shell touch " + marker },
			oneline: func(marker string) string { return ".shell touch " + marker },
		},
		{
			name:    "system",
			sql:     func(marker string) string { return ".system touch " + marker },
			oneline: func(marker string) string { return ".system touch " + marker },
		},
		{
			name:    "output",
			sql:     func(marker string) string { return ".output " + marker + "\nSELECT 42 AS a;" },
			oneline: func(marker string) string { return ".output " + marker },
		},
	}
	for _, c := range commands {
		for _, m := range modes {
			t.Run(c.name+"/"+m.label, func(t *testing.T) {
				marker := filepath.Join(LockdownDir(f.sibling), "dot-"+c.name+"-"+m.label+".txt")
				defer os.Remove(marker)
				payload := c.sql(marker)
				if m.label == "command-argument" {
					payload = c.oneline(marker)
				}
				res, err := m.run(payload)
				if err != nil {
					t.Fatal(err)
				}
				if _, statErr := os.Stat(marker); statErr != nil {
					t.Fatalf("the CLI refused .%s in %s mode under full lockdown (exit %d, output %q): this test pins the spike's negative result, so a failure here is good news — a mode exists in which the stdio transport may be confinable, and docs/design/lakehouse-plan.md section 2.2 mechanism 1 is worth re-evaluating. Re-evaluate only when every .command here is refused, not just this one", c.name, m.label, res.ExitCode, res.Combined())
				}
			})
		}
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
