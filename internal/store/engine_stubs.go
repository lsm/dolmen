package store

import (
	"context"
	"errors"
)

// errNotImplemented is the single placeholder error the two Engine methods
// whose concrete bodies arrive in later slices of the #159 plan return until
// then (plan slice 2b): ChangesSince (5c) and Listen (6b). Their signatures
// are pinned by the interface (2a), so each later slice swaps only the body —
// and nothing advertises a capability the engine does not have yet
// (Capabilities grew its real body in 4d, GetRows in 5a; notifications and
// subscribe stay false until Listen's body lands in 6b).
var errNotImplemented = errors.New("not implemented by the SQLite engine yet")

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
