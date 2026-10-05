package filter

import (
	"context"
	"errors"
	"testing"
)

func TestCanceledFilterDoesNotParse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Parse("body = 'value'", Options{Context: ctx, Columns: []string{"body"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("filter parsing after cancellation: %v", err)
	}
}
