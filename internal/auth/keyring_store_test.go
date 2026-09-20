package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestDeploymentIDIsMintedOnceAndPersists(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	r, err := OpenRegistry(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	first, err := r.DeploymentID(ctx, "")
	if err != nil {
		t.Fatalf("deployment id: %v", err)
	}
	r.Close()

	r2, err := OpenRegistry(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer r2.Close()
	again, err := r2.DeploymentID(ctx, "")
	if err != nil {
		t.Fatalf("deployment id: %v", err)
	}
	if again != first {
		t.Fatalf("the deployment id changed across a restart: %q then %q", first, again)
	}

	if _, err := r2.DeploymentID(ctx, "something-else"); err == nil {
		t.Fatal("a pinned deployment id that disagrees with the stored one was accepted")
	} else if !strings.Contains(err.Error(), first) {
		t.Fatalf("the startup error does not name the stored id: %v", err)
	}

	same, err := r2.DeploymentID(ctx, first)
	if err != nil || same != first {
		t.Fatalf("pinning the stored id was refused: %q %v", same, err)
	}
}

func TestSigningKeyPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	now := time.Now()

	r, err := OpenRegistry(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	dep, _ := r.DeploymentID(ctx, "")
	k, err := r.LoadKeyring(ctx, dep)
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	tok, err := MintToken(k, "oidc:v1:abc:sub", nil, DefaultTokenTTL, now)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	r.Close()

	r2, err := OpenRegistry(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer r2.Close()
	dep2, _ := r2.DeploymentID(ctx, "")
	k2, err := r2.LoadKeyring(ctx, dep2)
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	if k2.Active.ID != k.Active.ID {
		t.Fatal("a restart minted a new signing key, so every live token would stop verifying")
	}
	if _, err := VerifyToken(k2, tok, now); err != nil {
		t.Fatalf("a token minted before the restart no longer verifies: %v", err)
	}
}

func TestRotationKeepsVerifyingLiveTokens(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	now := time.Now()

	r, err := OpenRegistry(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()
	dep, _ := r.DeploymentID(ctx, "")
	k, _ := r.LoadKeyring(ctx, dep)
	tok, err := MintToken(k, "oidc:v1:abc:sub", nil, DefaultTokenTTL, now)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	rotated, err := r.RotateSigningKey(ctx, dep, false)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated.Active.ID == k.Active.ID {
		t.Fatal("rotation did not mint a successor")
	}
	if _, err := VerifyToken(rotated, tok, now); err != nil {
		t.Fatalf("a token from before the rotation stopped verifying during the overlap: %v", err)
	}
	fresh, err := MintToken(rotated, "oidc:v1:abc:sub", nil, DefaultTokenTTL, now)
	if err != nil {
		t.Fatalf("mint after rotation: %v", err)
	}
	if _, err := VerifyToken(rotated, fresh, now); err != nil {
		t.Fatalf("a token from the successor does not verify: %v", err)
	}
}

func TestRetiringPredecessorsRevokesTheirTokens(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	now := time.Now()

	r, err := OpenRegistry(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()
	dep, _ := r.DeploymentID(ctx, "")
	k, _ := r.LoadKeyring(ctx, dep)
	old, err := MintToken(k, "oidc:v1:abc:sub", nil, DefaultTokenTTL, now)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	rotated, err := r.RotateSigningKey(ctx, dep, true)
	if err != nil {
		t.Fatalf("rotate with retirement: %v", err)
	}
	if _, err := VerifyToken(rotated, old, now); !errors.Is(err, ErrTokenInvalid) {
		t.Fatal("retiring the predecessor left its tokens verifying, so rotation revokes nothing")
	}

	reopened, err := r.LoadKeyring(ctx, dep)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, err := VerifyToken(reopened, old, now); !errors.Is(err, ErrTokenInvalid) {
		t.Fatal("a retired key came back after reloading the keyring")
	}
	fresh, err := MintToken(reopened, "oidc:v1:abc:sub", nil, DefaultTokenTTL, now)
	if err != nil {
		t.Fatalf("mint after retirement: %v", err)
	}
	if _, err := VerifyToken(reopened, fresh, now); err != nil {
		t.Fatalf("the successor does not verify after retirement: %v", err)
	}
}

func TestKeyringRefreshPropagatesRetirement(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	now := time.Now()

	replicaA, err := OpenRegistry(dir)
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	defer replicaA.Close()
	dep, _ := replicaA.DeploymentID(ctx, "")
	ring, _ := replicaA.LoadKeyring(ctx, dep)
	token, err := MintToken(ring, "oidc:v1:abc:sub", nil, DefaultTokenTTL, now)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	other, err := New(Config{Mode: ModeOn})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	other.UseTokens(ring)
	other.RefreshTokensFrom(func(ctx context.Context) (Keyring, error) {
		return replicaA.LoadKeyring(ctx, dep)
	}, time.Nanosecond)

	if _, err := VerifyToken(other.mustRing(t), token, now); err != nil {
		t.Fatalf("the token should verify before retirement: %v", err)
	}

	if _, err := replicaA.RotateSigningKey(ctx, dep, true); err != nil {
		t.Fatalf("rotate with retirement: %v", err)
	}

	if _, err := VerifyToken(other.mustRing(t), token, now); !errors.Is(err, ErrTokenInvalid) {
		t.Fatal("a replica that did not serve the rotation kept honouring the retired key's tokens")
	}
}

func TestPendingSignInsAreCapped(t *testing.T) {
	r, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()
	ctx := context.Background()

	for i := 0; i < MaxPendingSignIns; i++ {
		if err := r.putPending(ctx, fmt.Sprintf("state-%d", i), "verifier", "https://example/cb"); err != nil {
			t.Fatalf("pending %d: %v", i, err)
		}
	}
	err = r.putPending(ctx, "one-too-many", "verifier", "https://example/cb")
	if err == nil {
		t.Fatal("an unauthenticated endpoint grew the registry without bound")
	}
	if !errors.Is(err, ErrAuthFlow) {
		t.Fatalf("the refusal is not a sign-in flow error: %v", err)
	}
}

func TestFirstBootMintsOneDeploymentAndOneActiveKey(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	a, err := OpenRegistry(dir)
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	defer a.Close()
	b, err := OpenRegistry(dir)
	if err != nil {
		t.Fatalf("open B: %v", err)
	}
	defer b.Close()

	idA, err := a.DeploymentID(ctx, "")
	if err != nil {
		t.Fatalf("A deployment id: %v", err)
	}
	idB, err := b.DeploymentID(ctx, "")
	if err != nil {
		t.Fatalf("B deployment id: %v", err)
	}
	if idA != idB {
		t.Fatalf("two replicas minted different deployment ids (%q and %q), so their tokens would never verify for each other", idA, idB)
	}

	ringA, err := a.LoadKeyring(ctx, idA)
	if err != nil {
		t.Fatalf("A keyring: %v", err)
	}
	ringB, err := b.LoadKeyring(ctx, idB)
	if err != nil {
		t.Fatalf("B keyring: %v", err)
	}
	if ringA.Active.ID != ringB.Active.ID {
		t.Fatalf("two replicas minted different active signing keys (%q and %q)", ringA.Active.ID, ringB.Active.ID)
	}
}
