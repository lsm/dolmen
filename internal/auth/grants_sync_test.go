package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTheGrantRegistrySurvivesPowerLoss(t *testing.T) {
	r, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var got int
	if err := r.db.QueryRow(`PRAGMA synchronous`).Scan(&got); err != nil || got != 2 {
		t.Fatalf("grant registry runs synchronous=%d (%v); a revocation must survive power loss, so it needs FULL (2)", got, err)
	}
}

func TestARelativeRegistryDirectoryStaysUnderTheWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.Mkdir("data", 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRegistry("data")
	if err != nil {
		t.Fatalf("a relative data directory must open, not be read as a URI authority: %v", err)
	}
	r.Close()
	if _, err := os.Stat(filepath.Join(dir, "data", RegistryFile)); err != nil {
		t.Fatalf("the registry must land in ./data, beside the namespaces: %v", err)
	}
}
