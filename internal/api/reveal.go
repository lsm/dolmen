package api

import (
	"context"
	"log/slog"

	"github.com/lsm/dolmen/internal/auth"
	"github.com/lsm/dolmen/internal/secret"
	"github.com/lsm/dolmen/internal/store"
)

func revealProp() map[string]any {
	return map[string]any{
		"type":        "array",
		"description": "Secret fields to return in plaintext; every other secret field reads as the mask \"" + secret.Mask + "\". Under -auth on this needs the reveal verb on the table, its namespace, or *, which admin does not imply, and it never widens the rows returned. Every reveal is written to the server's audit log, without the value",
		"items":       map[string]any{"type": "string"},
		"maxItems":    store.MaxFieldsPerTable,
		"uniqueItems": true,
	}
}

const revealDeniedMessage = "revealing a secret field needs the reveal verb on this table, its namespace, or *, and the caller does not hold it (admin does not imply it); an administrator can grant the reveal verb with the grant op, or omit reveal to read secret fields as the mask \"" + secret.Mask + "\""

const revealAdminKeyMessage = "the bootstrap admin key cannot reveal secret fields: its implicit admin does not imply the reveal verb, and reveal is never implied; grant the reveal verb to a real principal with the grant op and reveal as that principal, or omit reveal to read secret fields as the mask \"" + secret.Mask + "\""

func (s *Server) auditReveal(ctx context.Context, ns, table string, fields []string, rows []map[string]any) {
	if len(fields) == 0 {
		return
	}
	ids := make([]any, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r["id"])
	}
	attrs := make([]any, 0, 12)
	if s.authn.On() {
		attrs = append(attrs, "principal", auth.IdentityFrom(ctx).Principal)
	}
	attrs = append(attrs, "namespace", ns, "table", table, "row_ids", ids, "fields", fields, "request_id", RequestIDFrom(ctx))
	slog.InfoContext(ctx, "secret reveal", attrs...)
}
