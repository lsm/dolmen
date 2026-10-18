package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"github.com/lsm/dolmen/internal/auth"
	"github.com/lsm/dolmen/internal/lakehouse"
	"github.com/lsm/dolmen/internal/store"
)

func runBackup(args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("dolmen backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data", envOr("DOLMEN_DATA", "data", getenv), "data directory to back up; the server may keep running")
	outDir := fs.String("out", "", "directory to write the backup into; it must be new or empty")
	engine := fs.String("engine", envOr("DOLMEN_ENGINE", store.EngineSQLite, getenv), "storage engine of the data directory: sqlite or lakehouse, as given to the server")
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: dolmen backup -out DIR [-data DIR] [-engine sqlite|lakehouse]\n\nCopies every namespace, and the grant registry when there is one, as a consistent snapshot that writes do not disturb, and writes a manifest of sizes, checksums and catalog versions last.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := parseSubcommand(fs, args, stderr); err != nil {
		return err
	}
	if *outDir == "" {
		return usageError(fs, stderr, "-out is required: name a new or empty directory for the backup")
	}
	if err := backupEngine(func(string) string { return *engine }, "backup"); err != nil {
		return usageError(fs, stderr, err.Error())
	}
	backup := store.Backup
	switch *engine {
	case store.EngineLakehouse, store.EngineSQLite:
	default:
		return usageError(fs, stderr, fmt.Sprintf("-engine %q is not one backup handles; use sqlite or lakehouse", *engine))
	}
	lake, err := lakehouse.NamespacesIn(*dataDir)
	if err != nil {
		return fmt.Errorf("inspect data directory %s: %w", *dataDir, err)
	}
	lite, err := store.NamespacesIn(*dataDir)
	if err != nil {
		return fmt.Errorf("inspect data directory %s: %w", *dataDir, err)
	}
	switch {
	case *engine == store.EngineLakehouse && len(lite) > 0:
		return usageError(fs, stderr, fmt.Sprintf("%s holds SQLite namespaces (%s first), which a lakehouse backup would leave out; back up a directory that holds one engine's namespaces, with that engine's -engine", *dataDir, lite[0]))
	case *engine == store.EngineSQLite && len(lake) > 0:
		return usageError(fs, stderr, fmt.Sprintf("%s holds lakehouse namespaces (%s first); back it up with -engine lakehouse", *dataDir, lake[0]))
	case *engine == store.EngineLakehouse:
		backup = lakehouse.Backup
	}
	m, err := backup(context.Background(), *dataDir, *outDir, []string{auth.RegistryFile})
	if err != nil {
		return err
	}
	for _, f := range m.Files {
		fmt.Fprintf(stdout, "backed up %s (%d bytes)\n", backupLabel(f), f.Bytes)
	}
	if len(m.DataFiles) > 0 {
		fmt.Fprintf(stdout, "backed up %d table data and metadata files\n", len(m.DataFiles))
	}
	printBlobs(stdout, "backed up", m.Blobs)
	fmt.Fprintf(stdout, "wrote %s\n", filepath.Join(*outDir, store.BackupManifestFile))
	return nil
}

func runRestore(args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("dolmen restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data", envOr("DOLMEN_DATA", "data", getenv), "data directory to restore into; nothing in it is ever overwritten")
	fromDir := fs.String("from", "", "backup directory written by dolmen backup")
	engine := fs.String("engine", envOr("DOLMEN_ENGINE", store.EngineSQLite, getenv), "storage engine the restored data directory serves: sqlite or lakehouse, as given to the server")
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: dolmen restore -from DIR [-data DIR] [-engine sqlite|lakehouse]\n\nChecks every file in the backup against its manifest and SQLite's integrity check before writing anything, then moves each into place atomically. A file already in place is skipped, so an interrupted restore can be run again.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := parseSubcommand(fs, args, stderr); err != nil {
		return err
	}
	if *fromDir == "" {
		return usageError(fs, stderr, "-from is required: name the directory dolmen backup wrote")
	}
	if err := backupEngine(func(string) string { return *engine }, "restore"); err != nil {
		return usageError(fs, stderr, err.Error())
	}
	manifest, err := store.ReadBackupManifest(*fromDir)
	if err != nil {
		return err
	}
	if from := cmp.Or(manifest.Engine, store.EngineSQLite); from != *engine {
		return usageError(fs, stderr, fmt.Sprintf("%s is a %s backup and this restore is for the %s engine; restore it with -engine %s into a data directory that engine serves", *fromDir, from, *engine, from))
	}
	m, err := store.Restore(context.Background(), *fromDir, *dataDir)
	if err != nil {
		return err
	}
	for _, f := range m.Files {
		fmt.Fprintf(stdout, "restored %s\n", backupLabel(f))
	}
	printBlobs(stdout, "restored", m.Blobs)
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

func backupEngine(getenv func(string) string, what string) error {
	if getenv("DOLMEN_ENGINE") == store.EnginePostgres {
		return fmt.Errorf("%s works on a SQLite or lakehouse data directory; for a PostgreSQL deployment use pg_dump and pg_restore (see docs/postgresql-operations.md)", what)
	}
	return nil
}

func backupLabel(f store.BackupFile) string {
	if f.Namespace != "" {
		return "namespace " + f.Namespace
	}
	return f.Path
}

func printBlobs(w io.Writer, verb string, blobs []store.BackupBlob) {
	for i := 0; i < len(blobs); {
		ns, n, bytes := blobs[i].Namespace, 0, int64(0)
		for ; i < len(blobs) && blobs[i].Namespace == ns; i++ {
			n, bytes = n+1, bytes+blobs[i].Bytes
		}
		fmt.Fprintf(w, "%s %d files of namespace %s (%d bytes)\n", verb, n, ns, bytes)
	}
}
