package auth

type Reach struct {
	Header     bool
	OIDC       bool
	OIDCIssuer string
}

func (r Reach) PrincipalReachable(id string) bool {
	if r.Header {
		return true
	}
	if r.OIDC {
		digest, qualified := OIDCIssuerOf(id)
		return qualified && digest == r.OIDCIssuer
	}
	return false
}

func (r Reach) Any() bool { return r.Header || r.OIDC }

func (a *Authenticator) Reach() Reach {
	if a == nil {
		return Reach{}
	}
	return Reach{Header: a.HeaderSourceEnabled(), OIDC: a.OIDCEnabled(), OIDCIssuer: a.oidcIssuer}
}
