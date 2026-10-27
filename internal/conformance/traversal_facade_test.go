package conformance

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/lsm/dolmen"
)

func TestGraphTraversalFacade(t *testing.T) {
	if testEngine(t) != "sqlite" {
		t.Skip("SQLite graph adapter")
	}
	h := newHarness(t)
	relationSetup(t, h)
	h.mustHTTP("insert", map[string]any{"namespace": "acme", "table": "nodes", "records": []any{map[string]any{"label": "n3"}}})
	for _, pair := range [][2]int{{1, 2}, {2, 3}} {
		h.mustHTTP("relate", map[string]any{"namespace": "acme", "table": "links", "source": pair[0], "target": pair[1], "type": "k"})
	}
	s, err := dolmen.Open(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	source, target := dolmen.NodeRef{Table: " Nodes ", ID: 1}, dolmen.NodeRef{Table: "nodes", ID: 3}
	path, err := s.FindPath(t.Context(), "acme", source, target, dolmen.TraversalOptions{})
	want := []dolmen.NodeRef{{Table: "nodes", ID: 1}, {Table: "nodes", ID: 2}, {Table: "nodes", ID: 3}}
	if err != nil || path.Truncated || path.StopReason != "complete" || !reflect.DeepEqual(path.Nodes, want) || len(path.Edges) != 2 {
		t.Fatalf("path: %+v %v", path, err)
	}
	seeds := []dolmen.NodeRef{source, source}
	sub, err := s.Subgraph(t.Context(), "acme", seeds, dolmen.TraversalOptions{})
	if err != nil || sub.Truncated || !reflect.DeepEqual(sub.Nodes, want) || len(sub.Edges) != 2 || seeds[0] != source {
		t.Fatalf("subgraph: %+v %v", sub, err)
	}
	for _, reason := range []string{"hops", "nodes", "edges", "work", "elapsed"} {
		opts := dolmen.TraversalOptions{Direction: "out"}
		switch reason {
		case "hops":
			opts.MaxHops = 1
		case "nodes":
			opts.MaxNodes = 1
		case "edges":
			opts.MaxEdges = 1
		case "work":
			opts.MaxWork = 1
		case "elapsed":
			opts.MaxElapsed = time.Nanosecond
		}
		for _, path := range []bool{false, true} {
			var got dolmen.GraphResult
			if path {
				got, err = s.FindPath(t.Context(), "acme", source, target, opts)
			} else {
				got, err = s.Subgraph(t.Context(), "acme", seeds, opts)
			}
			if err != nil || !got.Truncated || got.StopReason != reason {
				t.Fatalf("%s/%v: %+v %v", reason, path, got, err)
			}
		}
	}
	if _, err = s.FindPath(t.Context(), "acme", source, target, dolmen.TraversalOptions{MaxWork: -1}); !errors.Is(err, dolmen.ErrInvalidRequest) {
		t.Fatalf("invalid work budget: %v", err)
	}
}
