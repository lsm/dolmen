package main

import (
	"flag"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/lsm/dolmen/internal/version"
)

func TestVersionFlagPrintsInjectedVersion(t *testing.T) {
	oldArgs, oldStdout := os.Args, os.Stdout
	defer func() { os.Args, os.Stdout = oldArgs, oldStdout }()
	os.Args = []string{"dolmen", "-version"}
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	runErr := run()
	w.Close()
	out, _ := io.ReadAll(r)
	if runErr != nil {
		t.Fatalf("run -version: %v", runErr)
	}
	if got, want := strings.TrimSpace(string(out)), "dolmen "+version.Version; got != want {
		t.Fatalf("--version printed %q, want %q", got, want)
	}
}
