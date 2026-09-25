package secret

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, KeySize) }

func seal(t *testing.T, k *Keyring, s string) []byte {
	t.Helper()
	b, err := k.Seal(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRoundTripAndFreshNonce(t *testing.T) {
	k, err := New(key(1))
	if err != nil {
		t.Fatal(err)
	}
	a, b := seal(t, k, "hunter2"), seal(t, k, "hunter2")
	if bytes.Equal(a, b) {
		t.Fatal("two seals of one value must differ")
	}
	if bytes.Contains(a, []byte("hunter2")) {
		t.Fatal("ciphertext carries the plaintext")
	}
	got, err := k.Open(a)
	if err != nil || got != "hunter2" {
		t.Fatalf("open = %q, %v", got, err)
	}
}

func TestWrongKeyTeaches(t *testing.T) {
	k1, _ := New(key(1))
	k2, _ := New(key(2))
	if _, err := k2.Open(seal(t, k1, "x")); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("err = %v", err)
	}
	var none *Keyring
	if _, err := none.Open(seal(t, k1, "x")); !errors.Is(err, ErrNoKey) {
		t.Fatalf("err = %v", err)
	}
	if _, err := none.Seal("x"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("err = %v", err)
	}
}

func TestTamperAndCorrupt(t *testing.T) {
	k, _ := New(key(1))
	b := seal(t, k, "x")
	b[len(b)-1] ^= 1
	if _, err := k.Open(b); !errors.Is(err, ErrTampered) {
		t.Fatalf("err = %v", err)
	}
	if _, err := k.Open([]byte("short")); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadKey(t *testing.T) {
	enc := "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="
	env := map[string]string{EnvKey: enc}
	get := func(s string) string { return env[s] }
	k, err := LoadKey(get)
	if err != nil || !bytes.Equal(k, key(1)) {
		t.Fatalf("k=%v err=%v", k, err)
	}
	p := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(p, []byte(enc+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env = map[string]string{EnvKeyFile: p}
	if k, err = LoadKey(get); err != nil || !bytes.Equal(k, key(1)) {
		t.Fatalf("file k=%v err=%v", k, err)
	}
	env = map[string]string{EnvKey: "c2hvcnQ="}
	if _, err = LoadKey(get); err == nil {
		t.Fatal("short key accepted")
	}
	env = map[string]string{EnvKey: enc, EnvKeyFile: p}
	if _, err = LoadKey(get); err == nil {
		t.Fatal("both sources accepted")
	}
	env = map[string]string{}
	if k, err = LoadKey(get); k != nil || err != nil {
		t.Fatal("no key must be nil, nil")
	}
}
