package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lsm/dolmen/internal/derr"
)

func TestQueryErrorClassifiesProgramLimitAsInvalid(t *testing.T) {
	err := queryError(context.Background(), &pgconn.PgError{Code: "54001", Message: "stack depth limit exceeded"})
	var de *derr.Error
	if !errors.As(err, &de) {
		t.Fatalf("want a *derr.Error, got %T: %v", err, err)
	}
	if de.Code != derr.InvalidRequest {
		t.Fatalf("SQLSTATE 54001 classified as %q, want invalid_request", de.Code)
	}
	if !strings.Contains(de.Message, "too large or complex") {
		t.Fatalf("message should explain the limit: %q", de.Message)
	}
}
