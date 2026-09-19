package auth

import (
	"github.com/lsm/dolmen/internal/derr"
)

const forbiddenMessage = "the request authenticated but the identity holds no grant: this build implements identity without grants, so the bootstrap admin key (DOLMEN_ADMIN_KEY) is the only credential with access; per-principal permissions arrive with the grant ops"

func (a *Authenticator) Authorize(id Identity) error {
	if !a.On() {
		return nil
	}
	if id.Principal == AdminPrincipal {
		return nil
	}
	return derr.New(derr.Forbidden, "%s", forbiddenMessage)
}
