package api

import (
	"net/http"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
)

func TestASQLEngineThatIsDownIsServiceUnavailable(t *testing.T) {
	status, code := statusFor(derr.Code("sql_engine_unavailable"))
	if status != http.StatusServiceUnavailable || code != "sql_engine_unavailable" {
		t.Fatalf("statusFor(sql_engine_unavailable) = %d %q, want 503 sql_engine_unavailable", status, code)
	}
}
