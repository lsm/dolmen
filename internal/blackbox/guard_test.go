package blackbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBlackBoxDisciplineNoInternalImports(t *testing.T) {
	internalNeedle := `"github.com/lsm/dolmen/` + `internal/`
	skillNeedle := `"github.com/lsm/dolmen/` + `skill`
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if !strings.HasSuffix(e.Name(), "_test.go") {
			t.Errorf("%s: blackbox package must contain *_test.go files only", e.Name())
		}
		raw, err := os.ReadFile(filepath.Join(".", e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		checked++
		if strings.Contains(string(raw), internalNeedle) {
			t.Errorf("%s: imports the dolmen internals; the blackbox suite speaks only the documented HTTP/MCP contract", e.Name())
		}
		if strings.Contains(string(raw), skillNeedle) {
			t.Errorf("%s: imports the dolmen skill package; skills are read over HTTP at runtime", e.Name())
		}
	}
	if checked == 0 {
		t.Fatal("no go source files found in the package directory")
	}
}
