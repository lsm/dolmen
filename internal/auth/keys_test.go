package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func seedKeyRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestKeyGenerationShape(t *testing.T) {
	secret, id, err := MintKey()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !ValidateKeyShape(secret) {
		t.Fatalf("minted key %q does not satisfy the pinned shape", secret)
	}
	if len(id) != KeyIDBytes*2 {
		t.Fatalf("key id %q", id)
	}
	other, _, _ := MintKey()
	if other == secret {
		t.Fatal("two mints produced the same credential")
	}
	for _, bad := range []string{"", "dlm_", "dlm_short", "dlm_" + strings.Repeat("a", 44), strings.Repeat("a", 47)} {
		if ValidateKeyShape(bad) {
			t.Fatalf("%q accepted as a key", bad)
		}
	}
}

func TestKeyRevocationCountsHeaderReachability(t *testing.T) {
	r := seedKeyRegistry(t)
	ctx := context.Background()
	root := Object{Namespace: RootObject}
	mustGrant(t, r, principal("boss"), root, VerbAdmin)
	k, _, err := r.CreateKey(ctx, "only", "boss", nil)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	if _, err := r.RevokeKey(ctx, k.ID, true, Reach{}); !errors.Is(err, ErrLastRootKey) {
		t.Fatalf("key-only deployment let its last root key go: %v", err)
	}
	if _, err := r.RevokeKey(ctx, k.ID, true, Reach{Header: true}); err != nil {
		t.Fatalf("a gateway can still assert boss, so dropping the compromised key must be allowed: %v", err)
	}
}

func TestKeyRevocationAllowsAReplacement(t *testing.T) {
	r := seedKeyRegistry(t)
	ctx := context.Background()
	root := Object{Namespace: RootObject}
	mustGrant(t, r, principal("boss"), root, VerbAdmin)
	first, _, _ := r.CreateKey(ctx, "first", "boss", nil)
	if _, _, err := r.CreateKey(ctx, "second", "boss", nil); err != nil {
		t.Fatalf("second key: %v", err)
	}
	if _, err := r.RevokeKey(ctx, first.ID, true, Reach{}); err != nil {
		t.Fatalf("a replacement key exists, so the revoke should be allowed: %v", err)
	}
}

func TestRootGroupGrantProvenByAKey(t *testing.T) {
	r := seedKeyRegistry(t)
	ctx := context.Background()
	mustGrant(t, r, group("ops"), Object{Namespace: RootObject}, VerbAdmin)
	k, _, err := r.CreateKey(ctx, "fleet", "bot", []string{"ops"})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	admins, _ := r.RootAdmins(ctx)
	keys, _ := r.ActiveKeys(ctx)
	if !rootReachableByKey(admins, keys, "") {
		t.Fatal("a key whose stored groups prove the root group grant was not counted")
	}
	if rootReachableByKey(admins, keys, k.ID) {
		t.Fatal("excluding the proving key still reported the group grant as reachable")
	}
}

func TestRevokedKeyStaysListedButInactive(t *testing.T) {
	r := seedKeyRegistry(t)
	ctx := context.Background()
	k, _, _ := r.CreateKey(ctx, "temp", "bot", nil)
	if _, err := r.RevokeKey(ctx, k.ID, false, Reach{}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	keys, err := r.ListKeys(ctx)
	if err != nil || len(keys) != 1 || !keys[0].Revoked {
		t.Fatalf("list after revoke: %+v %v", keys, err)
	}
	active, _ := r.ActiveKeys(ctx)
	if len(active) != 0 {
		t.Fatalf("a revoked key is still active: %+v", active)
	}
	if again, err := r.RevokeKey(ctx, k.ID, false, Reach{}); err != nil || !again.Revoked {
		t.Fatalf("re-revoking should succeed unchanged: %+v %v", again, err)
	}
	if missing, err := r.RevokeKey(ctx, "nosuchkey", false, Reach{}); err != nil || missing.ID != "" {
		t.Fatalf("revoking an unknown id should succeed with no key: %+v %v", missing, err)
	}
}

func TestStartupRequiresAReachableRootAdministrator(t *testing.T) {
	r := seedKeyRegistry(t)
	ctx := context.Background()
	mustGrant(t, r, principal("boss"), Object{Namespace: RootObject}, VerbAdmin)

	keysOnly, err := New(Config{Mode: ModeOn})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := keysOnly.CheckRootAdministrator(ctx, r); err == nil {
		t.Fatal("auth on with no source at all started")
	}

	keysOnly.UseKeys(r)
	if err := keysOnly.CheckRootAdministrator(ctx, r); err == nil {
		t.Fatal("a key-only deployment started with no key bearing the root principal")
	}
	if _, _, err := r.CreateKey(ctx, "boot", "boss", nil); err != nil {
		t.Fatalf("create key: %v", err)
	}
	if err := keysOnly.CheckRootAdministrator(ctx, r); err != nil {
		t.Fatalf("a key bearing the root principal should satisfy the check: %v", err)
	}
}
