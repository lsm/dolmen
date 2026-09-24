package store

import (
	"strings"
	"testing"
)

func TestAWindowsDrivePathIsAFileURIPathNotAnAuthority(t *testing.T) {
	got := dsn(`C:\data\ns.db`, true)
	if !strings.HasPrefix(got, "file:///C:") {
		t.Fatalf("a drive-letter path must render as file:///C:/..., not file://C:/... (SQLite reads C: as an authority): %s", got)
	}
	if got := dsn("/var/data/ns.db", true); !strings.HasPrefix(got, "file:///var/data/ns.db") {
		t.Fatalf("an absolute Unix path changed shape: %s", got)
	}
}
