package postgres

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lsm/dolmen/internal/derr"
)

func TestConnCapacityClassifiesExhaustionAsRetryable(t *testing.T) {
	for _, code := range []string{"53300", "57P03"} {
		err := connCapacity(fmt.Errorf("failed to connect: %w", &pgconn.PgError{Code: code, Message: "remaining connection slots are reserved"}))
		var de *derr.Error
		if !errors.As(err, &de) {
			t.Fatalf("%s: want a *derr.Error, got %T: %v", code, err, err)
		}
		if de.Code != derr.Timeout {
			t.Fatalf("%s: classified as %q, want timeout", code, de.Code)
		}
		if !strings.Contains(de.Message, "retry") || (code == "57P03") == strings.Contains(de.Message, "connection limit") {
			t.Fatalf("%s: message must tell the caller to retry: %q", code, de.Message)
		}
	}
	other := &pgconn.PgError{Code: "42703"}
	if got := connCapacity(other); got != other {
		t.Fatalf("an unrelated error must pass through unchanged, got %v", got)
	}
}
