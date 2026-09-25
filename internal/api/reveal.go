package api

import (
	"context"

	"github.com/lsm/dolmen/internal/secret"
	"github.com/lsm/dolmen/internal/store"
)

func revealProp() map[string]any {
	return map[string]any{
		"type":        "array",
		"description": "Secret fields to return in plaintext; every other secret field reads as the mask \"" + secret.Mask + "\". Allowed only while the server runs with -auth off",
		"items":       map[string]any{"type": "string"},
		"maxItems":    store.MaxFieldsPerTable,
		"uniqueItems": true,
	}
}

const revealUnderAuthMessage = "reveal is refused while the server runs with -auth on: revealing a secret needs the reveal verb, which this release does not grant yet; omit reveal to read secret fields as the mask \"" + secret.Mask + "\""

func (s *Server) revealContext(ctx context.Context, fields []string) (context.Context, error) {
	if len(fields) == 0 {
		return ctx, nil
	}
	if s.authn.On() {
		return nil, forbidden("%s", revealUnderAuthMessage)
	}
	return store.WithReveal(ctx, fields), nil
}
