package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/lsm/dolmen/internal/ops"
)

type relateReq struct {
	Namespace      string          `json:"namespace"`
	Table          string          `json:"table"`
	Source         json.RawMessage `json:"source"`
	Target         json.RawMessage `json:"target"`
	Type           *string         `json:"type"`
	Properties     map[string]any  `json:"properties"`
	IdempotencyKey json.RawMessage `json:"idempotency_key"`
}

var relateOp = OpDef{
	Description: "Insert one directed edge into a relation table: a row whose declared source and target fields hold the ids of the two rows it connects, " +
		"whose type field holds the edge's type, and whose other fields come from properties. Both endpoints must exist; a missing one is a conflict naming its foreign key, and nothing is written. " +
		"Pass idempotency_key to make a retry safe, exactly as for insert: the same key and edge return the original id with replayed=true. Delete edges with delete; an endpoint row cannot be deleted while an edge references it. " +
		fileWriteHint,
	InputSchema: map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"namespace":       nsProp("Namespace of the relation table"),
			"table":           existingTableProp("Relation table, one declared with relation {source, target, type}"),
			"source":          map[string]any{"type": "integer", "minimum": 1, "description": "Id of the row the edge starts at, in the table the source field's foreign key names"},
			"target":          map[string]any{"type": "integer", "minimum": 1, "description": "Id of the row the edge ends at, in the table the target field's foreign key names"},
			"type":            map[string]any{"type": "string", "minLength": 1, "description": "The edge's type, stored in the relation's type field"},
			"properties":      map[string]any{"type": "object", "description": "The edge's other fields, keyed by field name; source, target and type are set by the arguments above, not here"},
			"idempotency_key": Ops["insert"].InputSchema["properties"].(map[string]any)["idempotency_key"],
		},
		"required": []string{"namespace", "table", "source", "target", "type"},
	},
	OutputSchema: outSchema(map[string]any{
		"id":       prop("integer", "Row id of the edge (the original id when an idempotency_key replayed)"),
		"replayed": prop("boolean", "True when an idempotency_key replayed a previous relate, so nothing was written"),
	}, "id", "replayed"),
	Func: relateFunc,
}

func init() {
	Ops["relate"] = relateOp
}

func relateEndpoint(raw json.RawMessage, name string) (int64, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if len(raw) > 0 && dec.Decode(&v) == nil {
		if n, ok := v.(json.Number); ok {
			if i, err := n.Int64(); err == nil {
				return i, nil
			}
		}
	}
	return 0, badRequest("%s is required: the integer id of the row the edge %s", name, map[string]string{"source": "starts at", "target": "ends at"}[name])
}

func relateFunc(ctx context.Context, s *Server, body []byte) (any, error) {
	var req relateReq
	if err := decodeExactBody(body, &req); err != nil {
		return nil, err
	}
	source, err := relateEndpoint(req.Source, "source")
	if err != nil {
		return nil, err
	}
	target, err := relateEndpoint(req.Target, "target")
	if err != nil {
		return nil, err
	}
	if req.Type == nil {
		return nil, badRequest("type is required: the edge's relation type")
	}
	sc, _, err := s.eng.TableState(ctx, normNS(req.Namespace), normTable(req.Table), nil)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	rec, err := ops.RelationRecord(sc, source, target, *req.Type, req.Properties)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	insert := map[string]any{"namespace": req.Namespace, "table": req.Table, "records": []any{rec}}
	if req.IdempotencyKey != nil {
		insert["idempotency_key"] = req.IdempotencyKey
	}
	raw, err := json.Marshal(insert)
	if err != nil {
		return nil, fmt.Errorf("relate: %w", err)
	}
	out, err := Ops["insert"].Func(ctx, s, raw)
	if err != nil {
		return nil, err
	}
	res := out.(map[string]any)
	return map[string]any{"id": res["ids"].([]int64)[0], "replayed": res["replayed"] == true}, nil
}
