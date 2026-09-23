package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"github.com/lsm/dolmen/internal/auth"
	"github.com/lsm/dolmen/internal/store"
)

func runBackup(args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("dolmen backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data", envOr("DOLMEN_DATA", "data", getenv), "data directory to back up; the server may keep running")
	outDir := fs.String("out", "", "directory to write the backup into; it must be new or empty")
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: dolmen backup -out DIR [-data DIR]\n\nCopies every namespace, and the grant registry when there is one, as a consistent snapshot that writes do not disturb, and writes a manifest of sizes, checksums and catalog versions last.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := parseSubcommand(fs, args, stderr); err != nil {
		return err
	}
	if *outDir == "" {
		return usageError(fs, stderr, "-out is required: name a new or empty directory for the backup")
	}
	if err := sqliteDataOnly(getenv, "backup"); err != nil {
		return usageError(fs, stderr, err.Error())
	}
	m, err := store.Backup(context.Background(), *dataDir, *outDir, []string{auth.RegistryFile})
	if err != nil {
		return err
	}
	for _, f := range m.Files {
		fmt.Fprintf(stdout, "backed up %s (%d bytes)\n", backupLabel(f), f.Bytes)
	}
	fmt.Fprintf(stdout, "wrote %s\n", filepath.Join(*outDir, store.BackupManifestFile))
	return nil
}

func runRestore(args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("dolmen restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data", envOr("DOLMEN_DATA", "data", getenv), "data directory to restore into; nothing in it is ever overwritten")
	fromDir := fs.String("from", "", "backup directory written by dolmen backup")
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: dolmen restore -from DIR [-data DIR]\n\nChecks every file in the backup against its manifest and SQLite's integrity check before writing anything, then moves each into place atomically. A file already in place is skipped, so an interrupted restore can be run again.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := parseSubcommand(fs, args, stderr); err != nil {
		return err
	}
	if *fromDir == "" {
		return usageError(fs, stderr, "-from is required: name the directory dolmen backup wrote")
	}
	if err := sqliteDataOnly(getenv, "restore"); err != nil {
		return usageError(fs, stderr, err.Error())
	}
	m, err := store.Restore(context.Background(), *fromDir, *dataDir)
	if err != nil {
		return err
	}
	for _, f := range m.Files {
		fmt.Fprintf(stdout, "restored %s\n", backupLabel(f))
	}
	return nil
}

func parseSubcommand(fs *flag.FlagSet, args []string, stderr io.Writer) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return &printedError{err}
	}
	if fs.NArg() > 0 {
		return usageError(fs, stderr, fmt.Sprintf("unexpected positional argument(s): %q", fs.Args()))
	}
	return nil
}

func usageError(fs *flag.FlagSet, stderr io.Writer, msg string) error {
	e := errors.New(msg)
	fmt.Fprintf(stderr, "%v\n", e)
	fs.Usage()
	return &printedError{e}
}

func sqliteDataOnly(getenv func(string) string, what string) error {
	if getenv("DOLMEN_ENGINE") == store.EnginePostgres {
		return fmt.Errorf("%s works on a SQLite data directory; for a PostgreSQL deployment use pg_dump and pg_restore (see docs/postgresql-operations.md)", what)
	}
	return nil
}

func backupLabel(f store.BackupFile) string {
	if f.Namespace != "" {
		return "namespace " + f.Namespace
	}
	return f.Path
}
