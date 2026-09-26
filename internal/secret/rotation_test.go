package secret

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetiredKeysDecryptAndActiveSeals(t *testing.T) {
	old, _ := New(key(1))
	blob := seal(t, old, "hunter2")
	k, err := New(key(2), key(1), key(2))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := k.Open(blob); err != nil || got != "hunter2" {
		t.Fatalf("open under retired key = %q, %v", got, err)
	}
	fresh := seal(t, k, "x")
	if id, _ := KeyID(fresh); id != k.ID() {
		t.Fatalf("seal used key %s, want active %s", id, k.ID())
	}
	if len(k.IDs()) != 2 || len(k.Retired()) != 1 || k.Retired()[0].ID() != old.ID() {
		t.Fatalf("ids = %v", k.IDs())
	}
	only, _ := New(key(2))
	_, err = only.Open(blob)
	var wk *WrongKeyError
	if !errors.As(err, &wk) || wk.Stored != old.ID() || !strings.Contains(err.Error(), EnvOldKeys) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadOldKeys(t *testing.T) {
	a := "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
	b := "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI="
	env := map[string]string{EnvOldKeys: a + ", " + b}
	get := func(s string) string { return env[s] }
	keys, err := LoadOldKeys(get)
	if err != nil || len(keys) != 2 || !bytes.Equal(keys[1], key(2)) {
		t.Fatalf("keys=%v err=%v", keys, err)
	}
	p := filepath.Join(t.TempDir(), "old")
	if err := os.WriteFile(p, []byte(a+"\n"+b+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env = map[string]string{EnvOldKeysFile: p}
	if keys, err = LoadOldKeys(get); err != nil || len(keys) != 2 {
		t.Fatalf("keys=%v err=%v", keys, err)
	}
	env = map[string]string{EnvOldKeys: "nope"}
	if _, err = LoadOldKeys(get); err == nil || !strings.Contains(err.Error(), EnvOldKeys) {
		t.Fatalf("err = %v", err)
	}
	env = map[string]string{EnvOldKeys: a, EnvOldKeysFile: p}
	if _, err = LoadOldKeys(get); err == nil {
		t.Fatal("both set must be refused")
	}
}
