package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/lsm/dolmen/internal/derr"
)

const unauthorizedMessage = "authentication is required and this request carried no credential this server accepts; present one as the Authorization header, Bearer <credential>; every rejection answers the same way, so this message does not say which part failed"

func Unauthorized() error {
	return derr.New(derr.Unauthorized, "%s", unauthorizedMessage)
}

type Authenticator struct {
	mode  Mode
	admin Source
}

type Config struct {
	Mode     Mode
	AdminKey string
	Stdio    bool
}

func New(cfg Config) (*Authenticator, error) {
	a := &Authenticator{mode: cfg.Mode}
	if cfg.AdminKey != "" {
		if err := ValidateAdminKey(cfg.AdminKey); err != nil {
			return nil, err
		}
		a.admin = NewAdminKeySource(cfg.AdminKey)
	}
	if !cfg.Mode.On() {
		return a, nil
	}
	if cfg.Stdio {
		return nil, fmt.Errorf("auth is on, but the stdio MCP transport carries no per-request credential: a pipe cannot present one, and treating whoever launched the subprocess as %s would be an unaudited bypass; serve HTTP for an authenticated deployment, or run stdio with auth off (it is reachable only by the process that spawned it)", AdminPrincipal)
	}
	if a.admin == nil {
		return nil, fmt.Errorf("auth is on, but no identity source is configured, so every request would answer 401: set DOLMEN_ADMIN_KEY to the bootstrap credential (%s), which is the only source this build implements", AdminPrincipal)
	}
	return a, nil
}

func (a *Authenticator) Mode() Mode { return a.mode }

func (a *Authenticator) On() bool { return a != nil && a.mode.On() }

func (a *Authenticator) Authenticate(r *http.Request) (Identity, error) {
	if !a.On() {
		return Identity{}, nil
	}
	token, ok := BearerToken(r)
	if !ok {
		return Identity{}, Unauthorized()
	}
	switch {
	case strings.HasPrefix(token, KeyPrefix):
		return Identity{}, Unauthorized()
	case strings.Contains(token, "."):
		return Identity{}, Unauthorized()
	}
	if a.admin == nil {
		return Identity{}, Unauthorized()
	}
	id, ok := a.admin.Authenticate(r)
	if !ok {
		return Identity{}, Unauthorized()
	}
	return id, nil
}

type identityKey struct{}

func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

func IdentityFrom(ctx context.Context) Identity {
	id, _ := ctx.Value(identityKey{}).(Identity)
	return id
}
