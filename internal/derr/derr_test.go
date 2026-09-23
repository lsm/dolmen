package derr

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrorMatchesCategorySentinel(t *testing.T) {
	err := Wrap(Conflict, fmt.Errorf("inner"))
	if !errors.Is(err, ErrConflict) {
		t.Fatal("wrapped error must match its category sentinel")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatal("wrapped error must not match a different category sentinel")
	}
}

func TestErrorUnwrapsToCause(t *testing.T) {
	inner := errors.New("inner")
	err := Wrap(Conflict, inner)
	if !errors.Is(err, inner) {
		t.Fatal("wrapped error must unwrap to its cause")
	}
	var typed *Error
	if !errors.As(err, &typed) {
		t.Fatal("wrapped error must be reachable with errors.As")
	}
	if typed.Code != Conflict {
		t.Fatalf("expected code %q, got %q", Conflict, typed.Code)
	}
}

func TestErrorMessageIsCauseMessage(t *testing.T) {
	inner := fmt.Errorf("outer: %w", errors.New("inner"))
	err := Wrap(Conflict, inner)
	if err.Error() != inner.Error() {
		t.Fatalf("message must be the cause's message verbatim, got %q", err.Error())
	}
}

func TestSentinelsAreStableValues(t *testing.T) {
	if ErrConflict.Error() != "" || string(ErrConflict.Code) != "conflict" {
		t.Fatalf("conflict sentinel must carry the taxonomy code, got %+v", ErrConflict)
	}
	pairs := []struct {
		sentinel *Error
		code     Code
	}{
		{ErrInvalidRequest, InvalidRequest},
		{ErrNotFound, NotFound},
		{ErrQuery, Query},
		{ErrConflict, Conflict},
		{ErrForbidden, Forbidden},
		{ErrEmbedderUnavailable, EmbedderUnavailable},
		{ErrCanceled, Canceled},
		{ErrTimeout, Timeout},
		{ErrInternal, Internal},
	}
	for _, p := range pairs {
		if p.sentinel.Code != p.code {
			t.Fatalf("sentinel %v must carry code %q", p.sentinel, p.code)
		}
	}
}

func TestNewFormatsMessage(t *testing.T) {
	err := New(NotFound, "table %s.%s", "ns", "t")
	if err.Error() != "table ns.t" {
		t.Fatalf("unexpected message %q", err.Error())
	}
	if err.Code != NotFound || err.Cause != nil {
		t.Fatalf("unexpected error value %+v", err)
	}
}

func TestWrapNilCauseIsSafe(t *testing.T) {
	err := Wrap(NotFound, nil)
	if err.Error() != string(NotFound) || err.Cause != nil || err.Code != NotFound {
		t.Fatalf("nil cause must produce a safe value, got %+v", err)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatal("nil-cause wrap must still match its category sentinel")
	}
}
