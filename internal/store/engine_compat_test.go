package store

import (
	"context"
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

// TestListenRequiresNotify pins the one precondition on Listen's now-real
// body (6b retires the 2b stub): a nil notify callback is refused up
// front — a session that could never deliver a record must not mint
// cursors behind a caller that passed nothing to invoke.


func TestListenRequiresNotify(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, _, err := st.Listen(ctx, "test", "", "", [16]byte{}, nil, nil, nil); err == nil {
		t.Error("Listen with nil notify = nil error, want rejection")
	}
}

// TestCapabilities pins the SQLite engine's 4d self-description (§6.2, §7):
// vector search executes exact — ann_recall_bound explicitly null, never
// omitted — and notifications/subscribe stay false until Listen's body lands
// (6b flips both with the SSE route). The post-commit registry (notify.go)
// is internal plumbing; the capability answers "is Listen implemented?",
// and a stub must not be advertised as the real thing.
func TestCapabilities(t *testing.T) {
	st := openStore(t)
	got := st.Capabilities()
	if got.VectorExecution != VectorExact {
		t.Errorf("VectorExecution = %q, want %q (brute-force exact, §7)", got.VectorExecution, VectorExact)
	}
	if got.ANNRecallBound != nil {
		t.Errorf("ANNRecallBound = %v, want explicit null under exact execution", *got.ANNRecallBound)
	}
	if got.Notifications {
		t.Error("Notifications = true, want false until Listen is implemented (6b)")
	}
	if got.Subscribe {
		t.Error("Subscribe = true, want false until live streams are implemented (6b)")
	}
}
