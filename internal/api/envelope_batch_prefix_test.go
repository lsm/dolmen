package api

import (
	"fmt"
	"testing"

	"github.com/lsm/dolmen/internal/derr"
)

func TestWrapStoreErrKeepsOnlyTheBatchIndexPrefix(t *testing.T) {
	inner := &derr.Error{Code: derr.Query, Message: "invalid PostgreSQL SQL or value (SQLSTATE 42703)"}
	if got := wrapStoreErr(fmt.Errorf("writes[1]: %w", inner)).Message; got != "writes[1]: "+inner.Message {
		t.Fatalf("batch prefix lost: %q", got)
	}
	if got := wrapStoreErr(fmt.Errorf("insert into secret_table: %w", inner)).Message; got != inner.Message {
		t.Fatalf("a non-batch wrapper leaked into the message: %q", got)
	}
}
