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
