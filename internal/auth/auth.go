package auth

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

type Mode string

const (
	ModeOff Mode = "off"
	ModeOn  Mode = "on"
)

func ParseMode(raw string) (Mode, error) {
	switch raw {
	case "", string(ModeOff):
		return ModeOff, nil
	case string(ModeOn):
		return ModeOn, nil
	}
	return "", fmt.Errorf("invalid auth mode %q: must be off (default, no identity required) or on (deny-by-default, identity required)", raw)
}

func (m Mode) On() bool { return m == ModeOn }

const AdminPrincipal = "dolmen-admin"

const KeyPrefix = "dlm_"

type Identity struct {
	Principal string
	Groups    []string
	Source    string
}

func (i Identity) Empty() bool { return i.Principal == "" }

type Source interface {
	Name() string
	Authenticate(r *http.Request) (Identity, bool)
}

type BearerSource interface {
	Source
	Claims(token string) bool
}

var adminKeyRe = regexp.MustCompile(`^[A-Za-z0-9_-]{32,256}$`)

func ValidateAdminKey(key string) error {
	if strings.HasPrefix(key, KeyPrefix) {
		return fmt.Errorf("DOLMEN_ADMIN_KEY must not begin with %q: that prefix routes a bearer credential to the API key registry, which never retries the admin-key comparison, so the key would validate at startup and then never authenticate", KeyPrefix)
	}
	if !adminKeyRe.MatchString(key) {
		return fmt.Errorf("DOLMEN_ADMIN_KEY must be 32 to 256 characters of [A-Za-z0-9_-] (unpadded base64url, the Bearer token68 grammar): HTTP field parsing strips surrounding whitespace and proxies may reject other characters, so a broader charset admits a key that cannot be transmitted faithfully; generate one with: openssl rand -base64 32 | tr '+/' '-_' | tr -d '='")
	}
	return nil
}

type adminKeySource struct {
	key []byte
}

func NewAdminKeySource(key string) Source {
	return &adminKeySource{key: []byte(key)}
}

func (s *adminKeySource) Name() string { return "admin-key" }

func (s *adminKeySource) Authenticate(r *http.Request) (Identity, bool) {
	token, ok := BearerToken(r)
	if !ok {
		return Identity{}, false
	}
	if subtle.ConstantTimeCompare([]byte(token), s.key) != 1 {
		return Identity{}, false
	}
	return Identity{Principal: AdminPrincipal, Source: s.Name()}, true
}

func BearerToken(r *http.Request) (string, bool) {
	raw := r.Header.Get("Authorization")
	if raw == "" {
		return "", false
	}
	const scheme = "bearer "
	if len(raw) <= len(scheme) || !strings.EqualFold(raw[:len(scheme)], scheme) {
		return "", false
	}
	return strings.TrimSpace(raw[len(scheme):]), true
}
