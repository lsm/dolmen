package api

import (
	"context"
	"encoding/json"
	"github.com/lsm/dolmen/internal/graph"
	"github.com/lsm/dolmen/internal/ops"
	"github.com/lsm/dolmen/internal/schema"
	"github.com/lsm/dolmen/internal/store"
	"maps"
)

func registerSearchGraph(ref map[string]any, budget func(int) map[string]any) {
	props := maps.Clone(Ops["search_vector"].InputSchema["properties"].(map[string]any))
	for _, key := range []string{"table", "offset", "limit", "reveal", "include_hidden"} {
		delete(props, key)
	}
	props["tables"] = map[string]any{"type": "array", "minItems": 1, "maxItems": 100, "uniqueItems": true, "items": existingTableProp("Seed table")}
	props["top_k"] = budget(200)
	expansion := maps.Clone(Ops["subgraph"].InputSchema["properties"].(map[string]any))
	delete(expansion, "namespace")
	delete(expansion, "seeds")
	props["expansion"] = objectSchema(false, expansion, nil)
	outputs := maps.Clone(Ops["subgraph"].OutputSchema["properties"].(map[string]any))
	outputs["seeds"] = map[string]any{"type": "array", "items": outSchema(map[string]any{"ref": ref, "score": prop("number", "Semantic seed cosine similarity")}, "ref", "score")}
	outputs["skipped_vectors"] = prop("integer", "Unscorable vectors")
	input := outSchema(props, "namespace", "tables")
	input["oneOf"] = Ops["search_vector"].InputSchema["oneOf"]
	Ops["search_graph"] = OpDef{Description: "Semantic seeds ranked across tables, then bounded SQLite adjacency expansion in the same snapshot. query text uses text; raw vectors use vector. filter/args restrict seed properties before ranking; relation_types restrict expansion. top_k defaults to 10. Seeds retain cosine scores separately from connections; expansion uses subgraph bounds and defaults.", InputSchema: input, OutputSchema: outSchema(outputs, "seeds", "nodes", "edges", "truncated", "stop_reason", "skipped_vectors"), Func: searchGraphFunc}
}
func searchGraphFunc(ctx context.Context, s *Server, body []byte) (any, error) {
	if longestVector(body, schema.MaxVectorDim) > schema.MaxVectorDim {
		return nil, wrapStoreErr(ops.QueryVectorTooLong())
	}
	req := searchGraphReq{TopK: 10, Expansion: json.RawMessage(`{}`)}
	if err := decodeAllowNullArgs(body, &req); err != nil {
		return nil, err
	}
	if len(req.Tables) == 0 || len(req.Tables) > 100 || req.TopK < 1 || req.TopK > 200 {
		return nil, badRequest("use 1 to 100 tables and top_k from 1 to 200")
	}
	ex := traversalReq{Direction: "both"}
	if err := decodeExactBody(req.Expansion, &ex); err != nil {
		return nil, err
	}
	fields, err := probeObject(req.Expansion)
	if err != nil {
		return nil, err
	}
	for _, key := range fields.keys {
		if key == "namespace" || key == "seeds" || key == "source" || key == "target" {
			return nil, badRequest("expansion accepts only subgraph direction, relation_types and bounds")
		}
	}
	q, err := ex.query()
	if err != nil {
		return nil, err
	}
	ns := normNS(req.Namespace)
	if err := s.requireNamespaceVisible(ctx, ns); err != nil {
		return nil, err
	}
	eng, ok := s.eng.(interface {
		SearchGraph(context.Context, string, []string, store.VectorQuery, int, graph.TraversalQuery, store.GraphAccess) (store.GraphSearchResult, error)
	})
	if !ok {
		return nil, badRequest("search_graph requires the SQLite graph adapter")
	}
	for i, table := range req.Tables {
		table = normTable(table)
		req.Tables[i] = table
		authBody, _ := json.Marshal(grantObjectRaw{Namespace: ns, Table: table})
		if err := s.authorizeOp(ctx, "search_vector", authBody); err != nil {
			return nil, err
		}
		_, _, sc, err := s.resolveScopeState(ctx, ns, table)
		if err != nil {
			return nil, err
		}
		if err := s.checkFilter(ctx, sc, req.Filter, req.Args); err != nil {
			return nil, err
		}
	}
	query, err := ops.PrepareVectorQuery(ctx, s.eng, ns, req.Tables[0], ops.VectorQuery{Text: req.Text, Vec: req.Vector, Column: req.Column, Filter: req.Filter, Args: req.Args, MinScore: req.MinScore}, s.tracedEmbedder(), embedProviderHelp)
	if err != nil {
		return nil, wrapStoreErr(err)
	}

	out, err := eng.SearchGraph(ctx, ns, req.Tables, query, req.TopK, q, s.graphScope)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	return out, nil
}

type searchGraphReq struct {
	Namespace string          `json:"namespace"`
	Tables    []string        `json:"tables"`
	Text      string          `json:"text"`
	Vector    []float64       `json:"vector"`
	Column    string          `json:"column"`
	Filter    string          `json:"filter"`
	Args      []any           `json:"args"`
	MinScore  *float64        `json:"min_score"`
	TopK      int             `json:"top_k"`
	Expansion json.RawMessage `json:"expansion"`
}
