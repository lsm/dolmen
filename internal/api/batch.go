package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lsm/dolmen/internal/store"
)

var batchKinds = []store.BatchWriteKind{
	store.BatchWriteInsert,
	store.BatchWriteUpdate,
	store.BatchWriteDelete,
	store.BatchWriteUpsert,
	store.BatchWriteUpsertByKey,
}

var batchPerBatchFields = []string{"namespace", "idempotency_key", "dry_run", "limit", "confirm"}

func batchKindList() string {
	out := ""
	for i, k := range batchKinds {
		if i > 0 {
			out += ", "
		}
		out += string(k)
	}
	return out
}

func batchFieldRemedy(name string) string {
	if name == "dry_run" {
		return "not part of batch at all: a batch is all-or-nothing, so it cannot mix a preview with writes that commit, and there is no top-level dry_run to move it to. Use a separate query to see what a delete would match."
	}
	return "set once for the whole batch, not per write; move it to the top level"
}

func isBatchKind(kind store.BatchWriteKind) bool {
	for _, k := range batchKinds {
		if k == kind {
			return true
		}
	}
	return false
}

func isBatchPerBatchField(name string) bool {
	for _, f := range batchPerBatchFields {
		if f == name {
			return true
		}
	}
	return false
}

func batchProperties(def map[string]any, drop func(string) bool) (map[string]any, []string) {
	props, _ := def["properties"].(map[string]any)
	out := make(map[string]any, len(props))
	for name, sub := range props {
		if drop(name) {
			continue
		}
		out[name] = sub
	}
	raw, _ := def["required"].([]string)
	required := make([]string, 0, len(raw))
	for _, name := range raw {
		if drop(name) {
			continue
		}
		required = append(required, name)
	}
	return out, required
}

func batchWriteSchema(kind store.BatchWriteKind) map[string]any {
	def, ok := Ops[string(kind)]
	if !ok {
		return map[string]any{"type": "object"}
	}
	props, required := batchProperties(def.InputSchema, isBatchPerBatchField)
	props["kind"] = map[string]any{
		"type":        "string",
		"enum":        []any{string(kind)},
		"description": "The write kind, which also fixes the fields accepted here: these are the " + string(kind) + " operation's own fields, minus namespace, idempotency_key, limit and confirm, which are set once for the whole batch, and minus dry_run, which batch does not have.",
	}
	required = append(required, "kind")
	return objectSchema(false, props, required)
}

func batchResultSchema(kind store.BatchWriteKind) map[string]any {
	def, ok := Ops[string(kind)]
	if !ok {
		return map[string]any{"type": "object"}
	}
	props, required := batchProperties(def.OutputSchema, func(name string) bool { return name == "replayed" })
	props["kind"] = map[string]any{
		"type":        "string",
		"enum":        []any{string(kind)},
		"description": "The kind of the write this result belongs to, so a caller can pair it with its request without counting.",
	}
	return outSchema(props, append(required, "kind")...)
}

func batchOneOf(schemas []map[string]any) map[string]any {
	oneOf := make([]any, 0, len(schemas))
	for _, s := range schemas {
		oneOf = append(oneOf, map[string]any{
			"type":                 "object",
			"properties":           s["properties"],
			"required":             s["required"],
			"additionalProperties": false,
		})
	}
	return map[string]any{"anyOf": oneOf}
}

func batchWritesProperty() map[string]any {
	schemas := make([]map[string]any, 0, len(batchKinds))
	for _, k := range batchKinds {
		schemas = append(schemas, batchWriteSchema(k))
	}
	return map[string]any{
		"type":        "array",
		"description": "The writes to apply, in order, inside one transaction. Every write is one of the five write operations with that operation's own fields, except that namespace, idempotency_key, limit and confirm are set once for the whole batch and are refused here. dry_run is refused here and is not part of batch at all - a batch is all-or-nothing, so it cannot mix a preview with writes that commit, and there is no top-level dry_run to move it to.",
		"minItems":    1,
		"maxItems":    store.MaxWritesPerBatch,
		"items":       batchOneOf(schemas),
	}
}

func batchResultsProperty() map[string]any {
	schemas := make([]map[string]any, 0, len(batchKinds))
	for _, k := range batchKinds {
		schemas = append(schemas, batchResultSchema(k))
	}
	return map[string]any{
		"type":        "array",
		"description": "One result per write, in the order the writes were given, each carrying only the fields its own kind returns.",
		"items":       batchOneOf(schemas),
	}
}

var batchDescription = "Apply several writes to one namespace in a single transaction: either all of them commit, or none does. " +
	"Each entry in writes is one of the insert, update, delete, upsert or upsert_by_key operations with the same fields, " +
	"and the results come back in the order the writes were given. " +
	fmt.Sprintf("At most %d writes, and at most %d rows touched across the whole batch. ", store.MaxWritesPerBatch, store.MaxRowsTouchedPerBatch) +
	"Prefer one batch over several calls when the writes belong together, and several smaller batches over one large one when they do not."

func applyBatchSchemas() {
	def, ok := Ops["batch"]
	if !ok {
		return
	}
	def.Description = batchDescription
	def.Func = batchFunc
	def.InputSchema = map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"namespace": map[string]any{
				"type":        "string",
				"description": "The namespace every write applies to; a batch cannot span namespaces.",
				"pattern":     store.NSPathPattern(),
			},
			"writes": batchWritesProperty(),
			"idempotency_key": map[string]any{
				"type":        "string",
				"description": "Re-sending the identical body with the same key returns the stored results and writes nothing; the same key with a different body is a conflict. An empty string and an explicit null are both refused, because either would silently make a retry apply every write a second time - omit the field for a batch that should not be replayable.",
				"pattern":     fmt.Sprintf(`^[ -~]{1,%d}$`, store.MaxIdempotencyKeyLen),
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Match cap for every delete in the batch; 0, a negative and null are refused, and without it a delete matching more rows than the cap is refused rather than deleted. Set once for the batch, not per write.",
			},
			"confirm": map[string]any{
				"type":        "boolean",
				"description": "Allow a delete in the batch to delete more rows than limit allows; null is refused. Set once for the batch, not per write.",
			},
		},
		"required": []string{"namespace", "writes"},
	}
	def.OutputSchema = outSchema(map[string]any{
		"replayed": map[string]any{
			"type":        "boolean",
			"description": "True when an idempotency_key replayed a previous batch, so the results below are the ones that batch returned and nothing was written.",
		},
		"results": batchResultsProperty(),
	}, "replayed", "results")
	Ops["batch"] = def
}

func decodeBatchWrites(raw []json.RawMessage) ([]store.BatchWrite, error) {
	out := make([]store.BatchWrite, 0, len(raw))
	for i, item := range raw {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(item, &fields); err != nil {
			return nil, badRequest("writes[%d]: %s", i, err.Error())
		}
		rawKind, ok := fields["kind"]
		if !ok {
			return nil, badRequest("writes[%d]: kind is required, one of %s", i, batchKindList())
		}
		var kind store.BatchWriteKind
		if err := json.Unmarshal(rawKind, &kind); err != nil {
			return nil, badRequest("writes[%d]: kind must be one of %s", i, batchKindList())
		}
		if !isBatchKind(kind) {
			return nil, badRequest("writes[%d]: unknown write kind %q; one of %s", i, kind, batchKindList())
		}
		names := make([]string, 0, len(fields))
		for name := range fields {
			names = append(names, name)
		}
		sortStrings(names)
		for _, name := range names {
			if isBatchPerBatchField(name) {
				return nil, badRequest("writes[%d]: %s is %s", i, name, batchFieldRemedy(name))
			}
		}
		for _, name := range names {
			if !batchFieldAllowed(kind, name) {
				return nil, badRequest("writes[%d]: a %s write does not take %q; see the %s operation for the fields it accepts", i, kind, name, kind)
			}
		}
		dec := json.NewDecoder(bytes.NewReader(item))
		dec.UseNumber()
		var w store.BatchWrite
		if err := dec.Decode(&w); err != nil {
			return nil, badRequest("writes[%d]: %s", i, err.Error())
		}
		for j, r := range w.Records {
			if r == nil {
				return nil, badRequest("writes[%d]: records[%d] must be an object, not null", i, j)
			}
		}
		if err := scalarArgs(w.Args); err != nil {
			return nil, badRequest("writes[%d]: %s", i, err.Error())
		}
		w.Kind = kind
		w.Table = normTable(w.Table)
		out = append(out, w)
	}
	return out, nil
}

func batchFieldAllowed(kind store.BatchWriteKind, name string) bool {
	if name == "kind" {
		return true
	}
	def, ok := Ops[string(kind)]
	if !ok {
		return false
	}
	props, _ := def.InputSchema["properties"].(map[string]any)
	_, allowed := props[name]
	return allowed
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func batchResultFor(r store.BatchWriteResult) map[string]any {
	out := map[string]any{"kind": string(r.Kind)}
	if r.Ids != nil {
		out["ids"] = r.Ids
	}
	switch r.Kind {
	case store.BatchWriteInsert:
		out["inserted"] = r.Inserted
	case store.BatchWriteUpdate:
		out["updated"] = r.Updated
	case store.BatchWriteDelete:
		out["matched"] = r.Matched
		out["deleted"] = r.Deleted
	default:
		out["inserted"] = r.Inserted
		out["updated"] = r.Updated
	}
	return out
}

func batchTarget(ns, table string) authTarget {
	return authTarget{Namespace: normNS(ns), Table: normTable(table)}
}

func indexed(i int, err error) error {
	if err == nil {
		return nil
	}
	apiErr := wrapStoreErr(err)
	return &Error{
		Status:  apiErr.Status,
		Code:    apiErr.Code,
		Message: fmt.Sprintf("writes[%d]: %s", i, apiErr.Message),
		Cause:   apiErr.Cause,
	}
}

func (s *Server) authorizeBatch(ctx context.Context, ns string, writes []store.BatchWrite) error {
	for i, w := range writes {
		if w.Table == "" {
			return badRequest("writes[%d]: table is required", i)
		}
		if err := s.authorizeVerbsOn(ctx, string(w.Kind), nil, batchTarget(ns, w.Table)); err != nil {
			return indexed(i, err)
		}
	}
	return nil
}

type batchBody struct {
	Namespace      string            `json:"namespace"`
	Writes         []json.RawMessage `json:"writes"`
	IdempotencyKey json.RawMessage   `json:"idempotency_key"`
	Limit          json.RawMessage   `json:"limit"`
	Confirm        json.RawMessage   `json:"confirm"`
}

func parseBatch(body []byte) (string, []store.BatchWrite, batchBody, error) {
	var top batchBody
	if err := decodeExactBody(body, &top); err != nil {
		return "", nil, top, err
	}
	writes, err := decodeBatchWrites(top.Writes)
	if err != nil {
		return "", nil, top, err
	}
	if len(writes) == 0 {
		return "", nil, top, badRequest("writes is required, with at least one write")
	}
	return normNS(top.Namespace), writes, top, nil
}

func (s *Server) resolveBatchScopes(ctx context.Context, ns string, writes []store.BatchWrite) error {
	for i := range writes {
		scope, inc, _, err := s.resolveScopeState(ctx, ns, normTable(writes[i].Table))
		if err != nil {
			return indexed(i, err)
		}
		writes[i].Scope = scope
		writes[i].Incarnation = inc
	}
	return nil
}

func (s *Server) checkBatchFilters(ctx context.Context, ns string, writes []store.BatchWrite) error {
	for i, w := range writes {
		if strings.TrimSpace(w.Filter) == "" {
			continue
		}
		_, _, sc, err := s.resolveScopeState(ctx, ns, normTable(w.Table))
		if err != nil {
			return indexed(i, err)
		}
		if err := s.checkFilter(sc, w.Filter, w.Args); err != nil {
			return indexed(i, err)
		}
	}
	return nil
}

func batchOptions(ctx context.Context, s *Server, top batchBody) (store.BatchOpts, error) {
	key := ""
	if top.IdempotencyKey != nil {
		if bytes.Equal(bytes.TrimSpace(top.IdempotencyKey), []byte("null")) {
			return store.BatchOpts{}, badRequest("idempotency_key must be a string; omit the field for a batch that should not be replayable, because null would silently make a retry apply every write a second time")
		}
		if err := json.Unmarshal(top.IdempotencyKey, &key); err != nil {
			return store.BatchOpts{}, badRequest("idempotency_key must be a string: %s", err.Error())
		}
		if key == "" {
			return store.BatchOpts{}, badRequest("idempotency_key is empty; omit the field for a batch that should not be replayable, because an empty key would silently make a retry apply every write a second time")
		}
	}
	limit, err := parseOptPosInt(top.Limit, "limit")
	if err != nil {
		return store.BatchOpts{}, err
	}
	confirm, err := parseOptBool(top.Confirm, "confirm")
	if err != nil {
		return store.BatchOpts{}, err
	}
	return store.BatchOpts{Owner: s.writeOwner(ctx), IdempotencyKey: key, Limit: limit, Confirm: confirm}, nil
}

func batchFunc(ctx context.Context, s *Server, body []byte) (any, error) {
	ns, writes, top, err := parseBatch(body)
	if err != nil {
		return nil, err
	}
	opts, err := batchOptions(ctx, s, top)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNamespace(ctx, ns); err != nil {
		return nil, wrapStoreErr(err)
	}
	if err := s.resolveBatchScopes(ctx, ns, writes); err != nil {
		return nil, err
	}
	if err := s.checkBatchFilters(ctx, ns, writes); err != nil {
		return nil, err
	}
	nsInc := store.Incarnation{}
	for _, w := range writes {
		nsInc.NsGen = w.Incarnation.NsGen
		break
	}
	res, err := s.eng.Batch(ctx, ns, writes, opts, s.embedder(), nil, nsInc)
	if err != nil {
		return nil, wrapStoreErr(err)
	}
	out := make([]any, 0, len(res.Results))
	for _, r := range res.Results {
		out = append(out, batchResultFor(r))
	}
	return map[string]any{"replayed": res.Replayed, "results": out}, nil
}
