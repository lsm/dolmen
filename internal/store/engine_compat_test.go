package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// Slice 2b: *Store satisfies Engine. The assertion is the slice's
// compile-time proof, and it is the compile-drift guard: 2a pinned the
// interface ("later slices add implementations, never parameters"), so any
// re-signature of either side — exactly what the pin forbids — breaks this
// file's compilation instead of surfacing as a mismatch elsewhere.
var _ Engine = (*Store)(nil)

// TestEngineMethodSetMatchesStore is the runtime twin of the assertion above:
// every Engine method exists on *Store with the identical type, and a drift
// is reported naming the method (a change that still satisfies the interface
// through promotion, e.g. a method moved to an embedded type, trips this
// reflection check).
func TestEngineMethodSetMatchesStore(t *testing.T) {
	eng := reflect.TypeOf((*Engine)(nil)).Elem()
	concrete := reflect.TypeOf(&Store{})
	if concrete.NumMethod() < eng.NumMethod() {
		t.Fatalf("*Store has %d methods, Engine needs %d", concrete.NumMethod(), eng.NumMethod())
	}
	for i := 0; i < eng.NumMethod(); i++ {
		m := eng.Method(i)
		cm, ok := concrete.MethodByName(m.Name)
		if !ok {
			t.Fatalf("*Store is missing Engine method %s", m.Name)
		}
		// The concrete method's type carries *Store as its first (receiver)
		// argument; the interface's does not — prepend it before comparing.
		want := m.Type
		args := make([]reflect.Type, 0, want.NumIn()+1)
		args = append(args, concrete)
		for j := 0; j < want.NumIn(); j++ {
			args = append(args, want.In(j))
		}
		outs := make([]reflect.Type, want.NumOut())
		for j := 0; j < want.NumOut(); j++ {
			outs[j] = want.Out(j)
		}
		want = reflect.FuncOf(args, outs, false)
		if cm.Type != want {
			t.Fatalf("%s drifted from the Engine signature: store %s, engine %s", m.Name, cm.Type, want)
		}
	}
}

// TestEngineStubsNotImplemented pins the 2b stub contract: the five Engine
// methods whose bodies arrive in later slices (NamespaceState 4a,
// Capabilities 4d, GetRows 5a, ChangesSince 5c, Listen 6b) report
// not-implemented rather than half-working, and Capabilities returns the
// zero value — a stub must never be mistaken for a real self-description.
func TestEngineStubsNotImplemented(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, err := st.NamespaceState(ctx, "test", nil); !errors.Is(err, errNotImplemented) {
		t.Errorf("NamespaceState = %v, want errNotImplemented", err)
	}
	if _, err := st.GetRows(ctx, "test", "t", nil, nil, Incarnation{}); !errors.Is(err, errNotImplemented) {
		t.Errorf("GetRows = %v, want errNotImplemented", err)
	}
	if _, _, err := st.ChangesSince(ctx, "test", "", "", [16]byte{}, nil, Incarnation{}, Page{}); !errors.Is(err, errNotImplemented) {
		t.Errorf("ChangesSince = %v, want errNotImplemented", err)
	}
	if _, _, err := st.Listen(ctx, "test", "", "", [16]byte{}, nil, nil, nil); !errors.Is(err, errNotImplemented) {
		t.Errorf("Listen = %v, want errNotImplemented", err)
	}
	if got := (EngineCapabilities{}); got != st.Capabilities() {
		t.Errorf("Capabilities = %+v, want the zero value", st.Capabilities())
	}
}
