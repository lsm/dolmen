package api

import (
	"context"
	"encoding/json"
	"maps"
	"time"

	"github.com/lsm/dolmen/internal/graph"
	"github.com/lsm/dolmen/internal/store"
)

func registerTraversals(ref map[string]any, budget func(int) map[string]any) {
	for _, name := range []string{"find_path", "subgraph"} {
		props := maps.Clone(Ops["neighbors"].InputSchema["properties"].(map[string]any))
		delete(props, "node")
		props["max_hops"], props["max_work"] = budget(32), budget(1000)
		required := []string{"namespace", "seeds"}
		props["seeds"] = map[string]any{"type": "array", "minItems": 1, "maxItems": 1000, "items": ref}
		if name == "find_path" {
			delete(props, "seeds")
			props["source"], props["target"] = ref, ref
			required = []string{"namespace", "source", "target"}
		}
		outputs := maps.Clone(Ops["neighbors"].OutputSchema["properties"].(map[string]any))
		outputs["stop_reason"] = map[string]any{"type": "string", "enum": []string{"complete", "hops", "nodes", "edges", "work", "elapsed"}}
		Ops[name] = OpDef{Description: "Bounded SQLite graph traversal in one visible snapshot. find_path returns a shortest unweighted path in source-to-target order, or empty arrays if no path was established; subgraph returns sorted deduplicated references. Hidden intermediates never connect results. Defaults: both directions, 4 hops, 1000 nodes/edges/examined candidates and 1000ms. A frontier at max_hops or exhausted work returns truncated; a bounded miss is not proof of absence.", InputSchema: outSchema(props, required...), OutputSchema: outSchema(outputs, "nodes", "edges", "truncated", "stop_reason"), Func: traversalFunc(name)}
	}
}

type traversalReq struct {
	Namespace     string          `json:"namespace"`
	Seeds         []graph.Ref     `json:"seeds"`
	Source        graph.Ref       `json:"source"`
	Target        graph.Ref       `json:"target"`
	Direction     string          `json:"direction"`
	RelationTypes []string        `json:"relation_types"`
	MaxHops       json.RawMessage `json:"max_hops"`
	MaxNodes      json.RawMessage `json:"max_nodes"`
	MaxEdges      json.RawMessage `json:"max_edges"`
	MaxWork       json.RawMessage `json:"max_work"`
	MaxElapsed    json.RawMessage `json:"max_elapsed_ms"`
}

func traversalFunc(name string) func(context.Context, *Server, []byte) (any, error) {
	return func(ctx context.Context, s *Server, body []byte) (any, error) {
		req := traversalReq{Direction: "both"}
		if err := decodeExactBody(body, &req); err != nil {
			return nil, err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			return nil, err
		}
		_, seedsPresent := fields["seeds"]
		_, sourcePresent := fields["source"]
		_, targetPresent := fields["target"]
		if (name == "find_path" && seedsPresent) || (name == "subgraph" && (sourcePresent || targetPresent)) {
			return nil, badRequest("find_path accepts source/target; subgraph accepts seeds")
		}
		limits := []int{4, 1000, 1000, 1000, 1000}
		for i, p := range []struct {
			raw  json.RawMessage
			name string
			max  int
		}{{req.MaxHops, "max_hops", 32}, {req.MaxNodes, "max_nodes", 1000}, {req.MaxEdges, "max_edges", 1000}, {req.MaxWork, "max_work", 1000}, {req.MaxElapsed, "max_elapsed_ms", 30000}} {
			n, err := parseOptPosInt(p.raw, p.name)
			if err != nil {
				return nil, err
			}
			if n > p.max {
				return nil, badRequest("%s must be at most %d", p.name, p.max)
			}
			if n != 0 {
				limits[i] = n
			}
		}
		q := graph.TraversalQuery{Seeds: req.Seeds, Direction: req.Direction, RelationTypes: req.RelationTypes, MaxHops: limits[0], MaxWork: limits[3], Limits: graph.Limits{Nodes: limits[1], Edges: limits[2], Elapsed: time.Duration(limits[4]) * time.Millisecond}}
		if name == "find_path" {
			q.Seeds = []graph.Ref{req.Source}
			q.Target = &req.Target
		}
		for i := range q.Seeds {
			q.Seeds[i].Table = normTable(q.Seeds[i].Table)
		}
		if q.Target != nil {
			q.Target.Table = normTable(q.Target.Table)
		}
		ns := normNS(req.Namespace)
		if err := s.requireNamespaceVisible(ctx, ns); err != nil {
			return nil, err
		}
		index, ok := s.eng.(interface {
			GraphIndex(store.GraphAccess) graph.Index
		})
		if !ok {
			return nil, badRequest("graph traversal requires the SQLite graph adapter")
		}
		out, err := graph.Traverse(ctx, index.GraphIndex(s.graphScope), ns, q)
		if err != nil {
			return nil, wrapStoreErr(err)
		}
		return out, nil
	}
}
