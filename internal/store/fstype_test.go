package store

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOpenWarnsWhenTheDataDirectoryIsOnANetworkFilesystem(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	prevDetect := detectNetworkFS
	t.Cleanup(func() { detectNetworkFS = prevDetect })

	detectNetworkFS = func(string) (string, bool) { return "nfs", true }
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if out := buf.String(); !strings.Contains(out, "network filesystem") || !strings.Contains(out, "filesystem=nfs") {
		t.Fatalf("a data directory on NFS must be named in a startup warning, logged:\n%s", out)
	}

	buf.Reset()
	detectNetworkFS = func(string) (string, bool) { return "", false }
	st, err = Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if strings.Contains(buf.String(), "network filesystem") {
		t.Fatalf("a local data directory must not warn:\n%s", buf.String())
	}
}

func TestTheTestDirectoryIsNotReportedAsANetworkFilesystem(t *testing.T) {
	if fs, remote := networkFilesystem(t.TempDir()); remote {
		t.Fatalf("the local temp dir was detected as %q", fs)
	}
}

func TestAnUnwritableDataDirectoryIsNamedBeforeAnyNamespaceOpens(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory write permission is not enforced here")
	}
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	err := probeWritable(dir)
	if err == nil || !strings.Contains(err.Error(), "not writable") || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("an unwritable data directory must be refused with the remediation: %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := probeWritable(dir); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("the probe left files behind: %v", entries)
	}
}
