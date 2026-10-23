package store

import (
	"fmt"
	"github.com/lsm/dolmen/internal/derr"
)

func CheckBatchExpectedRows(w BatchWrite, result BatchWriteResult) error {
	if w.ExpectedRows == nil {
		return nil
	}
	matched := result.Updated
	if w.Kind == BatchWriteDelete {
		matched = result.Matched
	}
	if matched != *w.ExpectedRows {
		return derr.New(derr.Conflict, "expected_rows mismatch; re-read state and retry the decision")
	}
	return nil
}

func RejectBatchExpectedRows(writes []BatchWrite, backend string) error {
	for i, w := range writes {
		if w.ExpectedRows != nil {
			return fmt.Errorf("writes[%d]: %w", i, invalidf("expected_rows is not yet supported by %s; use a SQLite batch until this backend supports conditional writes", backend))
		}
	}
	return nil
}
