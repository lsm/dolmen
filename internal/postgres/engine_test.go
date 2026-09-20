package postgres

import (
	"testing"

	"github.com/lsm/dolmen/internal/store"
)

var _ store.Engine = (*Store)(nil)

func TestStoreSatisfiesEngine(t *testing.T) {
	var engine store.Engine = (*Store)(nil)
	if engine == nil {
		t.Fatal("postgres store does not satisfy store.Engine")
	}
}
