package store

import (
	"context"
	"errors"
)

// errNotImplemented is the single placeholder error the four Engine methods
// whose concrete bodies arrive in later slices of the #159 plan return until
// then (plan slice 2b): Capabilities (4d), GetRows (5a), ChangesSince (5c),
// and Listen (6b). Their signatures are pinned by the interface (2a), so
// each later slice swaps only the body — and nothing advertises a capability
// the engine does not have yet (Capabilities reports the zero value for the
// same reason: the stub must not be mistaken for a real "exact engine"
// self-description).
var errNotImplemented = errors.New("not implemented by the SQLite engine yet")

// GetRows is the id-addressed scoped fetch behind read_rows (§2, §6.2). Stub
// — slice 5a implements it.
func (s *Store) GetRows(ctx context.Context, ns, table string, ids []int64, scope *RowScope, scopeIncarnation Incarnation) (QueryResult, error) {
	return QueryResult{}, errNotImplemented
}

// ChangesSince is the scoped replay read over the durable change log
// (§6.2, §9.3). Stub — slice 5c implements it (with the log itself).
func (s *Store) ChangesSince(ctx context.Context, ns, table string, from Cursor, nsGen [16]byte, scope *RowScope, scopeIncarnation Incarnation, page Page) ([]ChangeRecord, Cursor, error) {
	return nil, "", errNotImplemented
}

// Listen is the engine-declared notification capability (§6.2, §9.3). Stub —
// slice 6b implements it.
func (s *Store) Listen(ctx context.Context, ns, table string, from Cursor, nsGen [16]byte, liveAuthz func(table string) (scope *RowScope, inc Incarnation, ok bool), notify func(ChangeRecord), closed func(cause error)) (*ChangeReplay, func(), error) {
	return nil, nil, errNotImplemented
}

// Capabilities is the engine's static self-description (§6.2). Stub — slice
// 4d replaces the zero value with the real facts.
func (s *Store) Capabilities() EngineCapabilities {
	return EngineCapabilities{}
}
