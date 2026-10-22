package dolmen

import (
	"errors"
	"strings"
	"testing"
)

func TestWithVectorCacheBytesRefusesANegativeSize(t *testing.T) {
	_, err := Open(t.TempDir(), WithVectorCacheBytes(-1))
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "WithVectorCacheBytes") {
		t.Fatalf("a negative cache size must be refused naming the option: %v", err)
	}
	st, err := Open(t.TempDir(), WithVectorCacheBytes(0))
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
}

func TestWithEngineRefusesAnUnimplementedEngine(t *testing.T) {
	for _, name := range []string{"postgres", "lakehouse"} {
		st, err := Open(t.TempDir(), WithEngine(name))
		if st != nil {
			st.Close()
			t.Fatalf("WithEngine(%q) opened a store, want a refusal", name)
		}
		if !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("WithEngine(%q) = %v, want invalid_request", name, err)
		}
		if !strings.Contains(err.Error(), "WithEngine") {
			t.Fatalf("WithEngine(%q) refusal %q does not name the option", name, err)
		}
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("WithEngine(%q) refusal %q does not name the engine", name, err)
		}
	}
}
