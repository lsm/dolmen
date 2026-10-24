package graphbench

import (
	"context"
	"time"
)

type Edge struct {
	ID, From, To uint64
	Kind         uint8
}

type Index interface {
	Append(context.Context, []Edge) error
	Snapshot(context.Context) (View, error)
	Close() error
}

type View interface {
	Adjacent(context.Context, uint64, bool, int) ([]Edge, bool, error)
	Close() error
}

type Query struct {
	Seeds              []uint64
	Target             uint64
	Hops, Nodes, Edges int
	Elapsed            time.Duration
	Reverse            bool
	Kind               uint8
	Visible            func(uint64) bool
	EdgeVisible        func(Edge) bool
}

type Result struct {
	Nodes, EdgeIDs, Path []uint64
	Scanned              int
	Truncated            bool
	Stop                 string
}
