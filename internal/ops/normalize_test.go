package ops

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
	"github.com/lsm/dolmen/internal/embed"
	"github.com/lsm/dolmen/internal/store"
)

func TestClassifyMatchesTheWireTaxonomy(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want derr.Code
	}{
		{"nil defaults to internal", nil, derr.Internal},
		{"shared invalid", derr.New(derr.InvalidRequest, "x"), derr.InvalidRequest},
		{"typed conflict from the engine", fmt.Errorf("outer: %w", derr.Wrap(derr.Conflict, fmt.Errorf("%w: key reuse", store.ErrInvalid))), derr.Conflict},
		{"query error", store.NewQueryError("SELECT 1", errors.New("unrecognized token: \"x\"")), derr.Query},
		{"query error naming a missing table", store.NewQueryError("SELECT 1", errors.New("no such table: gone")), derr.NotFound},
		{"not found", fmt.Errorf("%w: namespace x", store.ErrNotFound), derr.NotFound},
		{"version conflict", &store.VersionConflictError{}, derr.Conflict},
		{"invalid", fmt.Errorf("%w: bad input", store.ErrInvalid), derr.InvalidRequest},
		{"load error", &embed.LoadError{Model: "m"}, derr.EmbedderUnavailable},
		{"provider error", &ProviderError{Cause: errors.New("upstream 429")}, derr.EmbedderUnavailable},
		{"load error wrapped by the provider boundary", &ProviderError{Cause: &embed.LoadError{Model: "m"}}, derr.EmbedderUnavailable},
		{"canceled", context.Canceled, derr.Canceled},
		{"deadline", context.DeadlineExceeded, derr.Timeout},
		{"wrapped deadline", fmt.Errorf("outer: %w", context.DeadlineExceeded), derr.Timeout},
		{"unknown", errors.New("mystery"), derr.Internal},
	}
	for _, tc := range cases {
		if got := Classify(tc.err); got != tc.want {
			t.Errorf("%s: Classify = %s, want %s", tc.name, got, tc.want)
		}
	}
}
