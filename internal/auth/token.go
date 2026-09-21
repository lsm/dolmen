package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	TokenType    = "dolmen-token"
	TokenAlg     = "EdDSA"
	TokenVersion = 1

	DefaultTokenTTL = 168 * time.Hour
	MinTokenTTL     = time.Hour
	MaxTokenTTL     = 720 * time.Hour
)

var ErrTokenInvalid = errors.New("token is not valid for this deployment")

type tokenHeader struct {
	Typ string `json:"typ"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

type tokenClaims struct {
	V   int      `json:"v"`
	Iss string   `json:"iss"`
	Sub string   `json:"sub"`
	Grp []string `json:"grp"`
	Iat int64    `json:"iat"`
	Exp int64    `json:"exp"`
}

type SigningKey struct {
	ID      string
	Private ed25519.PrivateKey
	Public  ed25519.PublicKey
	Retired bool
}

type Keyring struct {
	Deployment string
	Active     SigningKey
	Verify     []SigningKey
}

func ValidateTokenTTL(d time.Duration) error {
	if d < MinTokenTTL || d > MaxTokenTTL {
		return fmt.Errorf("invalid token ttl %s: must be between %s and %s", d, MinTokenTTL, MaxTokenTTL)
	}
	return nil
}

func NewSigningKey() (SigningKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return SigningKey{}, fmt.Errorf("generate signing key: %w", err)
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return SigningKey{}, fmt.Errorf("generate key id: %w", err)
	}
	return SigningKey{ID: hex.EncodeToString(id[:]), Private: priv, Public: pub}, nil
}

func NewDeploymentID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate deployment id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func b64(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }

func MintToken(k Keyring, principal string, groups []string, ttl time.Duration, now time.Time) (string, error) {
	if k.Deployment == "" {
		return "", fmt.Errorf("the deployment has no issuer id, so a token could not be bound to it")
	}
	if groups == nil {
		groups = []string{}
	}
	header, err := json.Marshal(tokenHeader{Typ: TokenType, Alg: TokenAlg, Kid: k.Active.ID})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(tokenClaims{
		V:   TokenVersion,
		Iss: k.Deployment,
		Sub: principal,
		Grp: groups,
		Iat: now.Unix(),
		Exp: now.Add(ttl).Unix(),
	})
	if err != nil {
		return "", err
	}
	signing := b64(header) + "." + b64(claims)
	sig := ed25519.Sign(k.Active.Private, []byte(signing))
	return signing + "." + b64(sig), nil
}

func LooksLikeToken(bearer string) bool {
	return strings.Count(bearer, ".") == 2
}

func VerifyToken(k Keyring, token string, now time.Time) (Identity, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Identity{}, ErrTokenInvalid
	}
	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Identity{}, ErrTokenInvalid
	}
	var h tokenHeader
	if err := json.Unmarshal(rawHeader, &h); err != nil {
		return Identity{}, ErrTokenInvalid
	}
	if h.Typ != TokenType || h.Alg != TokenAlg {
		return Identity{}, ErrTokenInvalid
	}
	pub, ok := publicKeyFor(k, h.Kid)
	if !ok {
		return Identity{}, ErrTokenInvalid
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Identity{}, ErrTokenInvalid
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return Identity{}, ErrTokenInvalid
	}
	rawClaims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Identity{}, ErrTokenInvalid
	}
	var c tokenClaims
	if err := json.Unmarshal(rawClaims, &c); err != nil {
		return Identity{}, ErrTokenInvalid
	}
	if c.V != TokenVersion {
		return Identity{}, ErrTokenInvalid
	}
	if subtle.ConstantTimeCompare([]byte(c.Iss), []byte(k.Deployment)) != 1 {
		return Identity{}, ErrTokenInvalid
	}
	if c.Exp <= now.Unix() {
		return Identity{}, ErrTokenInvalid
	}
	if c.Sub == "" || c.Sub == AdminPrincipal || !principalRe.MatchString(c.Sub) {
		return Identity{}, ErrTokenInvalid
	}
	for _, g := range c.Grp {
		if !groupRe.MatchString(g) {
			return Identity{}, ErrTokenInvalid
		}
	}
	return Identity{Principal: c.Sub, Groups: c.Grp, Source: OIDCSourceName}, nil
}

func publicKeyFor(k Keyring, kid string) (ed25519.PublicKey, bool) {
	if k.Active.ID == kid && k.Active.Public != nil {
		return k.Active.Public, true
	}
	for _, v := range k.Verify {
		if v.ID == kid && v.Public != nil {
			return v.Public, true
		}
	}
	return nil, false
}

func signEd25519(k Keyring, signing string) []byte {
	return ed25519.Sign(k.Active.Private, []byte(signing))
}
