package store

import (
	"context"
	"reflect"
	"testing"
)

var _ Engine = (*Store)(nil)

func TestValidateEngine(t *testing.T) {
	for _, name := range []string{"", EngineSQLite} {
		if err := ValidateEngine(name); err != nil {
			t.Fatalf("ValidateEngine(%q) = %v, want nil", name, err)
		}
	}
	err := ValidateEngine(EnginePostgres)
	if err == nil || err.Error() != `unknown engine "postgres" (the available engine is "sqlite")` {
		t.Fatalf("ValidateEngine(postgres) = %v, want the teaching error", err)
	}
}

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

func TestListenRequiresNotify(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, _, err := st.Listen(ctx, "test", "", "", [16]byte{}, nil, nil, nil); err == nil {
		t.Error("Listen with nil notify = nil error, want rejection")
	}
}

func TestCapabilities(t *testing.T) {
	st := openStore(t)
	got := st.Capabilities()
	if got.VectorExecution != VectorExact {
		t.Errorf("VectorExecution = %q, want %q (brute-force exact, §7)", got.VectorExecution, VectorExact)
	}
	if got.ANNRecallBound != nil {
		t.Errorf("ANNRecallBound = %v, want explicit null under exact execution", *got.ANNRecallBound)
	}
	if !got.Notifications {
		t.Error("Notifications = false, want true since 6b implemented Listen")
	}
	if !got.Subscribe {
		t.Error("Subscribe = false, want true since 6b registered the /v1/subscribe stream")
	}
}
