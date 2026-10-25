package api

import (
	"context"
	"encoding/json"
	"time"

	"github.com/lsm/dolmen/internal/auth"
	"github.com/lsm/dolmen/internal/graph"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
)

func init() {
	ref := outSchema(map[string]any{"table": existingTableProp("Table containing the row"), "id": map[string]any{"type": "integer", "minimum": 1}}, "table", "id")
	budget := func(max int) map[string]any { return map[string]any{"type": "integer", "minimum": 1, "maximum": max} }
	Ops["neighbors"] = OpDef{
		Description:  "Read visible one-hop relation and endpoint references in stable table/id order from one SQLite snapshot. Hidden edges and endpoints never connect results. Defaults: both directions, 1000 nodes, 1000 examined edges, 1000ms. A budget stop returns truncated with stop_reason nodes, edges or elapsed; it is not proof of absence. Read row properties with read_rows.",
		InputSchema:  outSchema(map[string]any{"namespace": nsProp("Namespace of the node"), "node": ref, "direction": map[string]any{"type": "string", "enum": []string{"out", "in", "both"}}, "relation_types": map[string]any{"type": "array", "maxItems": 1000, "items": prop("string", "Relation type")}, "max_nodes": budget(1000), "max_edges": budget(1000), "max_elapsed_ms": budget(30000)}, "namespace", "node"),
		OutputSchema: outSchema(map[string]any{"nodes": map[string]any{"type": "array", "items": ref}, "edges": map[string]any{"type": "array", "items": outSchema(map[string]any{"ref": ref, "source": ref, "target": ref, "type": prop("string", "Relation type")}, "ref", "source", "target", "type")}, "truncated": prop("boolean", "True when a work budget stopped the read"), "stop_reason": map[string]any{"type": "string", "enum": []string{"complete", "nodes", "edges", "elapsed"}}}, "nodes", "edges", "truncated", "stop_reason"),
		Func:         neighborsFunc,
	}
}

type neighborsReq struct {
	Namespace     string          `json:"namespace"`
	Node          graph.Ref       `json:"node"`
	Direction     string          `json:"direction"`
	RelationTypes []string        `json:"relation_types"`
	MaxNodes      json.RawMessage `json:"max_nodes"`
	MaxEdges      json.RawMessage `json:"max_edges"`
	MaxElapsed    json.RawMessage `json:"max_elapsed_ms"`
}

func neighborsFunc(ctx context.Context, s *Server, body []byte) (any, error) {
	req := neighborsReq{Direction: "both"}
	if err := decodeExactBody(body, &req); err != nil {
		return nil, err
	}
	limits := []int{1000, 1000, 1000}
	for i, p := range []struct {
		raw  json.RawMessage
		name string
		max  int
	}{{req.MaxNodes, "max_nodes", 1000}, {req.MaxEdges, "max_edges", 1000}, {req.MaxElapsed, "max_elapsed_ms", 30000}} {
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
	ns := normNS(req.Namespace)
	req.Node.Table = normTable(req.Node.Table)
	if err := s.requireNamespaceVisible(ctx, ns); err != nil {
		return nil, err
	}
	index, ok := s.eng.(interface {
		GraphIndex(store.GraphAccess) graph.Index
	})
	if !ok {
		return nil, badRequest("neighbors requires the SQLite graph adapter")
	}
	res, err := graph.Neighbors(ctx, index.GraphIndex(s.graphScope), ns, graph.Query{Node: req.Node, Direction: req.Direction, RelationTypes: req.RelationTypes, Limits: graph.Limits{Nodes: limits[0], Edges: limits[1], Elapsed: time.Duration(limits[2]) * time.Millisecond}})
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	return res, nil
}

func (s *Server) graphScope(ctx context.Context, sc *schema.TableSchema) (*store.RowScope, error) {
	if !s.authn.On() {
		return nil, nil
	}
	id := auth.IdentityFrom(ctx)
	if id.Principal == auth.AdminPrincipal {
		return nil, nil
	}
	if s.grants == nil {
		return nil, errNoGrantRegistry
	}
	verbs, err := s.grants.EffectiveVerbs(ctx, id, auth.Object{Namespace: sc.Namespace, Table: sc.Name})
	if err != nil {
		return nil, err
	}
	if verbs.Has(auth.VerbRead) {
		return nil, nil
	}
	if sc.RowAccess == schema.RowAccessOwn && verbs.HasAny(dataVerbs...) {
		return &store.RowScope{Owner: id.Principal}, nil
	}
	return &store.RowScope{Empty: true}, nil
}
