package store

import "github.com/lsm/dolmen/internal/derr"

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
