package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/auth"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func TestBackupThenRestoreRoundTripsADataDirectory(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.CreateNamespace(ctx, "research", [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTable(ctx, "research", "findings", []schema.Field{{Name: "title", Type: schema.String}}, store.TableOpts{}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	reg, err := auth.OpenRegistry(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	reg.Close()

	env := func(string) string { return "" }
	outDir := filepath.Join(t.TempDir(), "backup")
	var stdout, stderr bytes.Buffer
	if err := runBackup([]string{"-data", dataDir, "-out", outDir}, env, &stdout, &stderr); err != nil {
		t.Fatalf("backup: %v\n%s", err, stderr.String())
	}
	for _, want := range []string{"backed up namespace research", "backed up " + auth.RegistryFile, "wrote " + filepath.Join(outDir, store.BackupManifestFile)} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("backup output lacks %q:\n%s", want, stdout.String())
		}
	}

	target := t.TempDir()
	stdout.Reset()
	if err := runRestore([]string{"-from", outDir, "-data", target}, env, &stdout, &stderr); err != nil {
		t.Fatalf("restore: %v\n%s", err, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(target, auth.RegistryFile)); err != nil {
		t.Fatalf("the grant registry must come back with the namespaces: %v", err)
	}
	restored, err := store.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	tables, err := restored.ListTables(ctx, "research", nil)
	if err != nil || len(tables) != 1 || tables[0] != "findings" {
		t.Fatalf("restored tables %v %v", tables, err)
	}
}

func TestBackupAndRestoreNameWhatTheyNeed(t *testing.T) {
	env := func(string) string { return "" }
	pg := func(k string) string {
		if k == "DOLMEN_ENGINE" {
			return store.EnginePostgres
		}
		return ""
	}
	cases := []struct {
		name string
		run  func() error
		want string
	}{
		{"backup without -out", func() error { return runBackup(nil, env, &bytes.Buffer{}, &bytes.Buffer{}) }, "-out is required"},
		{"restore without -from", func() error { return runRestore(nil, env, &bytes.Buffer{}, &bytes.Buffer{}) }, "-from is required"},
		{"backup on PostgreSQL", func() error {
			return runBackup([]string{"-out", t.TempDir()}, pg, &bytes.Buffer{}, &bytes.Buffer{})
		}, "use pg_dump"},
		{"restore of a directory that is not a backup", func() error {
			return runRestore([]string{"-from", t.TempDir(), "-data", t.TempDir()}, env, &bytes.Buffer{}, &bytes.Buffer{})
		}, "not a finished backup"},
	}
	for _, c := range cases {
		if err := c.run(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error naming %q", c.name, err, c.want)
		}
	}
}
