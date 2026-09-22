package store

import (
	"context"
	"errors"
	"testing"

	"github.com/lsm/dolmen/internal/schema"
)

func TestATokenRoundTripsThroughItsEncoding(t *testing.T) {
	want := Incarnation{NsGen: [16]byte{1, 2, 3, 250}, Version: 7, DropGen: 3, Table: "notes"}
	got, err := DecodeIncarnation(EncodeIncarnation(want))
	if err != nil {
		t.Fatalf("decode a token this server minted: %v", err)
	}
	if got != want {
		t.Fatalf("the token decoded to %+v, want %+v", got, want)
	}
}

func TestAPlanMintsATokenThatNamesItsNamespace(t *testing.T) {
	st := openRowAccessStore(t)
	seedTwoOwners(t, st)
	plan, err := st.PlanMigration(context.Background(), "ns", "notes", []schema.Change{
		{Op: schema.OpAddField, Field: &schema.Field{Name: "tag", Type: schema.String}, Default: "x"},
	}, Embedder{}, Incarnation{}, nil, Incarnation{})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	minted, err := DecodeIncarnation(plan.ExpectedIncarnation)
	if err != nil {
		t.Fatalf("a token the server just minted must decode: %v", err)
	}
	if minted.NsGen == ([16]byte{}) {
		t.Fatal("a minted token carries the namespace generation, so refusing an all-zero one cannot reject a real plan's token")
	}
}

func TestAHandCraftedTokenWithoutANamespaceGenerationIsRefused(t *testing.T) {
	forged := EncodeIncarnation(Incarnation{Version: 1, Table: "notes"})
	if _, err := DecodeIncarnation(forged); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an all-zero namespace generation is one no server mints, and accepting it turns the token back into the version-only precondition the API refuses under authentication: %v", err)
	}
}
