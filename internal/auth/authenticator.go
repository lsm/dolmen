package auth

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/lsm/dolmen/internal/derr"
)

const unauthorizedMessage = "authentication is required and this request carried no credential this server accepts; present one as the Authorization header, Bearer <credential>; every rejection answers the same way, so this message does not say which part failed"

func Unauthorized() error {
	return derr.New(derr.Unauthorized, "%s", unauthorizedMessage)
}

type Authenticator struct {
	mode   Mode
	admin  Source
	header Source
	keys   Source

	maxGroups int
}

func (a *Authenticator) UseKeys(r *Registry) {
	if a == nil || r == nil {
		return
	}
	a.keys = &keySource{reg: r}
}

type Config struct {
	Mode           Mode
	AdminKey       string
	TrustedProxies []*net.IPNet
	MaxGroups      int
	Stdio          bool
}

func New(cfg Config) (*Authenticator, error) {
	a := &Authenticator{mode: cfg.Mode, maxGroups: cfg.MaxGroups}
	if cfg.AdminKey != "" {
		if err := ValidateAdminKey(cfg.AdminKey); err != nil {
			return nil, err
		}
		a.admin = NewAdminKeySource(cfg.AdminKey)
	}
	if cfg.MaxGroups != 0 {
		if err := ValidateMaxGroups(cfg.MaxGroups); err != nil {
			return nil, err
		}
	}
	if len(cfg.TrustedProxies) > 0 {
		a.header = NewHeaderSource(cfg.TrustedProxies, cfg.MaxGroups)
	}
	if !cfg.Mode.On() {
		return a, nil
	}
	if cfg.Stdio {
		return nil, fmt.Errorf("auth is on, but the stdio MCP transport carries no per-request credential: a pipe cannot present one, and treating whoever launched the subprocess as %s would be an unaudited bypass; serve HTTP for an authenticated deployment, or run stdio with auth off (it is reachable only by the process that spawned it)", AdminPrincipal)
	}
	if a.admin == nil && a.header == nil {
		return nil, fmt.Errorf("auth is on, but no identity source is configured, so every request would answer 401: set DOLMEN_ADMIN_KEY to the bootstrap credential (%s), or DOLMEN_TRUSTED_PROXIES to accept identity asserted by a gateway", AdminPrincipal)
	}
	return a, nil
}

func (a *Authenticator) Mode() Mode { return a.mode }

func (a *Authenticator) AdminKeyConfigured() bool { return a != nil && a.admin != nil }

func (a *Authenticator) MaxGroups() int { return a.maxGroups }

func (a *Authenticator) On() bool { return a != nil && a.mode.On() }

func (a *Authenticator) Authenticate(r *http.Request) (Identity, error) {
	if !a.On() {
		return Identity{}, nil
	}
	token, ok := BearerToken(r)
	if !ok {
		if a.header != nil {
			if id, ok := a.header.Authenticate(r); ok {
				return id, nil
			}
		}
		return Identity{}, Unauthorized()
	}
	switch {
	case strings.HasPrefix(token, KeyPrefix):
		if a.keys == nil {
			return Identity{}, Unauthorized()
		}
		id, ok := a.keys.Authenticate(r)
		if !ok {
			return Identity{}, Unauthorized()
		}
		return id, nil
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

type RootAdminSource interface {
	RootAdmins(ctx context.Context) ([]Subject, error)
}

func (a *Authenticator) CheckRootAdministrator(ctx context.Context, src RootAdminSource) error {
	if !a.On() || a.AdminKeyConfigured() {
		return nil
	}
	admins, err := src.RootAdmins(ctx)
	if err != nil {
		return err
	}
	for _, s := range admins {
		if s.Type == SubjectPrincipal {
			return nil
		}
	}
	return fmt.Errorf("auth is on but the deployment has no usable root administrator: no DOLMEN_ADMIN_KEY is set and no principal holds admin on \"*\", so nobody could grant anything; set DOLMEN_ADMIN_KEY and restart, which restores the bootstrap administrator while existing grants persist")
}
