package api

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/lsm/dolmen/internal/store"
)

const (
	rotateDefaultLimit = 10000
	rotateMaxLimit     = 100000
)

type rotateReq struct {
	Namespace *string `json:"namespace"`
	Limit     *int    `json:"limit"`
}

var rotateSecretKeyOp = OpDef{
	Description: "Re-encrypt stored secret values under the server's active secret key (DOLMEN_SECRET_KEY), after the operator made a new key active and listed the previous one in DOLMEN_SECRET_KEYS_OLD. " +
		"Writes already use the active key; this moves existing values over in short transactions of a few hundred values each, so writers are never blocked for long. " +
		"Each call does a bounded amount of work (at most limit values, and at most about half the operation timeout) and returns what it did and what remains; call it again until done is true. " +
		"It is idempotent and resumable: every value stays decryptable under one of the configured keys at every point, so an interrupted call is simply called again. " +
		"keys counts the values still stored under each key id; a retired key is safe to remove from DOLMEN_SECRET_KEYS_OLD once its count is 0 on every namespace. " +
		"namespace limits the work and the report to one namespace (not its children); without it every namespace is covered. " +
		"A value under a key id that is not configured stops the call with conflict naming the key id to add back. " +
		"Needs admin on the whole server (*).",
	InputSchema: map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"namespace": nsProp("Only rotate this namespace; omit for every namespace"),
			"limit": map[string]any{
				"type":        "integer",
				"description": "Most values to re-encrypt in this call (default 10000, max 100000)",
				"minimum":     1,
				"maximum":     rotateMaxLimit,
			},
		},
	},
	OutputSchema: outSchema(map[string]any{
		"active_key": prop("string", "Id of the active key every rotated value is now encrypted under"),
		"rotated":    map[string]any{"type": "integer", "minimum": 0, "description": "Values re-encrypted by this call"},
		"remaining":  map[string]any{"type": "integer", "minimum": 0, "description": "Values still under a key other than the active one; call again while it is above 0"},
		"done":       propBool(),
		"keys": map[string]any{
			"type":        "array",
			"description": "Stored secret values per key id across the covered namespaces, the active key included",
			"items": outSchema(map[string]any{
				"key_id": prop("string", "Key id (first 8 bytes of SHA-256 of the key, hex)"),
				"active": propBool(),
				"values": map[string]any{"type": "integer", "minimum": 0, "description": "Values stored under this key"},
			}, "key_id", "active", "values"),
		},
		"tables": map[string]any{
			"type":        "array",
			"description": "Every table holding a secret field in the covered namespaces",
			"items": outSchema(map[string]any{
				"namespace": prop("string", "Namespace"),
				"table":     prop("string", "Table"),
				"rotated":   map[string]any{"type": "integer", "minimum": 0, "description": "Values re-encrypted in this table by this call"},
				"remaining": map[string]any{"type": "integer", "minimum": 0, "description": "Values in this table still under a non-active key"},
			}, "namespace", "table", "rotated", "remaining"),
		},
	}, "active_key", "rotated", "remaining", "done", "keys", "tables"),
	Func: func(ctx context.Context, s *Server, body []byte) (any, error) {
		var req rotateReq
		if err := decode(body, &req); err != nil {
			return nil, err
		}
		active := s.eng.SecretKeyID()
		if active == "" {
			return nil, wrapStoreErr(store.RotationNeedsKey())
		}
		opts := store.RotateOpts{Limit: rotateDefaultLimit}
		if req.Limit != nil {
			if *req.Limit < 1 || *req.Limit > rotateMaxLimit {
				return nil, badRequest("limit must be between 1 and %d", rotateMaxLimit)
			}
			opts.Limit = *req.Limit
		}
		if dl, ok := ctx.Deadline(); ok {
			opts.Until = time.Now().Add(time.Until(dl) / 2)
		}
		var nss []string
		if req.Namespace != nil {
			ns := normNS(*req.Namespace)
			if ns == "" {
				return nil, badRequest("namespace must not be empty; omit it to rotate every namespace")
			}
			nss = []string{ns}
		} else {
			all, err := s.eng.ListNamespaces(ctx, "", nil)
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			nss = all
		}
		tables := []any{}
		keys := map[string]int64{}
		var rotated, remaining int64
		for _, ns := range nss {
			left := opts
			if opts.Limit > 0 {
				left.Limit = opts.Limit - int(rotated)
				if left.Limit <= 0 {
					left.Limit = -1
				}
			}
			res, err := s.eng.RotateSecrets(ctx, ns, left)
			if err != nil && req.Namespace == nil && errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, wrapStoreErr(err)
			}
			for _, t := range res.Tables {
				tables = append(tables, map[string]any{"namespace": ns, "table": t.Table, "rotated": t.Rotated, "remaining": t.Remaining})
				for id, n := range t.Keys {
					keys[id] += n
				}
			}
			rotated += res.Rotated()
			remaining += res.Remaining()
		}
		ids := make([]string, 0, len(keys))
		for id := range keys {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		keyList := make([]any, 0, len(ids))
		for _, id := range ids {
			keyList = append(keyList, map[string]any{"key_id": id, "active": id == active, "values": keys[id]})
		}
		return map[string]any{"active_key": active, "rotated": rotated, "remaining": remaining, "done": remaining == 0, "keys": keyList, "tables": tables}, nil
	},
}
