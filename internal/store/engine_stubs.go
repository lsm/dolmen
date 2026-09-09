package store

import (
	"context"
	"errors"
)

// errNotImplemented is the single placeholder error the one Engine method
// whose concrete body arrives in a later slice of the #159 plan returns until
// then (plan slice 2b): Listen (6b). Its signature is pinned by the interface
// (2a), so the later slice swaps only the body — and nothing advertises a
// capability the engine does not have yet (Capabilities grew its real body in
// 4d, GetRows in 5a, ChangesSince in 5c; notifications and subscribe stay
// false until Listen's body lands in 6b).
var errNotImplemented = errors.New("not implemented by the SQLite engine yet")

// Listen is the engine-declared notification capability (§6.2, §9.3). Stub —
// slice 6b implements it.
func (s *Store) Listen(ctx context.Context, ns, table string, from Cursor, nsGen [16]byte, liveAuthz func(table string) (scope *RowScope, inc Incarnation, ok bool), notify func(ChangeRecord), closed func(cause error)) (*ChangeReplay, func(), error) {
	return nil, nil, errNotImplemented
}
