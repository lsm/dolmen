package store

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestARelativeDataDirectoryResolvesUnderTheWorkingDirectory(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	want := "file://" + sqliteURIPath(filepath.Join(wd, "data", "ns.db"))
	if got := dsn(filepath.Join("data", "ns.db"), true); !strings.HasPrefix(got, want) {
		t.Fatalf("a relative data directory must resolve under the working directory, not the root or a URI authority: %s, want prefix %s", got, want)
	}
	if !strings.HasPrefix(want, "file:///") {
		t.Fatalf("a file URI path must start at the root: %s", want)
	}
}

func TestAWindowsDrivePathIsAFileURIPathNotAnAuthority(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("drive letters exist only on Windows")
	}
	if got := dsn(`C:\data\ns.db`, true); !strings.HasPrefix(got, "file:///C:/data/ns.db") {
		t.Fatalf("a drive-letter path must render as file:///C:/..., not file://C:/... (SQLite reads C: as an authority): %s", got)
	}
}
