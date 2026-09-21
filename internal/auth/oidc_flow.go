package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	AuthBeginPath    = "/v1/auth/begin"
	AuthCallbackPath = "/v1/auth/callback"

	pendingTTL = 10 * time.Minute
)

var ErrAuthFlow = errors.New("the sign-in could not be completed")

type providerEndpoints struct {
	Authorize string
	Token     string
	UserInfo  string
}

type discoveryDoc struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

type OIDCSource struct {
	cfg    OIDCConfig
	reg    *Registry
	ring   Keyring
	client *http.Client
	digest string

	mu        sync.Mutex
	endpoints providerEndpoints
	onRing    func(Keyring)
	ringGen   uint64

	rotateMu sync.Mutex
}

func (s *OIDCSource) PublishRingTo(f func(Keyring)) {
	s.mu.Lock()
	s.onRing = f
	s.mu.Unlock()
}

func NewOIDCSource(cfg OIDCConfig, reg *Registry, ring Keyring, client *http.Client) *OIDCSource {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &OIDCSource{cfg: cfg, reg: reg, ring: ring, client: client, digest: IssuerDigest(cfg.IssuerKey())}
}

func (s *OIDCSource) Name() string { return OIDCSourceName }

func (s *OIDCSource) IssuerDigest() string { return s.digest }

func (s *OIDCSource) resolveEndpoints(ctx context.Context) (providerEndpoints, error) {
	s.mu.Lock()
	cached := s.endpoints
	s.mu.Unlock()
	if cached.Authorize != "" {
		return cached, nil
	}
	if s.cfg.Preset == PresetGitHub {
		eps := providerEndpoints{Authorize: githubAuthorizeURL, Token: githubTokenURL, UserInfo: githubUserURL}
		if err := eps.validate(); err != nil {
			return providerEndpoints{}, err
		}
		return s.cacheEndpoints(eps), nil
	}
	docURL := strings.TrimRight(s.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, docURL, nil)
	if err != nil {
		return providerEndpoints{}, err
	}
	res, err := s.client.Do(req)
	if err != nil {
		return providerEndpoints{}, fmt.Errorf("%w: the identity provider's discovery document could not be fetched", ErrAuthFlow)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return providerEndpoints{}, fmt.Errorf("%w: the identity provider's discovery document answered %d", ErrAuthFlow, res.StatusCode)
	}
	var doc discoveryDoc
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&doc); err != nil {
		return providerEndpoints{}, fmt.Errorf("%w: the identity provider's discovery document is not valid JSON", ErrAuthFlow)
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" {
		return providerEndpoints{}, fmt.Errorf("%w: the identity provider's discovery document names no authorization or token endpoint", ErrAuthFlow)
	}
	eps := providerEndpoints{
		Authorize: doc.AuthorizationEndpoint,
		Token:     doc.TokenEndpoint,
		UserInfo:  doc.UserinfoEndpoint,
	}
	if err := eps.validate(); err != nil {
		return providerEndpoints{}, err
	}
	return s.cacheEndpoints(eps), nil
}

func (e providerEndpoints) validate() error {
	for _, ep := range []struct {
		name     string
		raw      string
		required bool
	}{
		{"authorization", e.Authorize, true},
		{"token", e.Token, true},
		{"userinfo", e.UserInfo, false},
	} {
		if ep.raw == "" && !ep.required {
			continue
		}
		u, err := url.Parse(ep.raw)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("%w: the identity provider's %s endpoint is %q, which is not an https URL; dolmen will not send the client secret or read identity claims over a connection a network attacker can read and rewrite", ErrAuthFlow, ep.name, ep.raw)
		}
	}
	return nil
}

func (s *OIDCSource) cacheEndpoints(e providerEndpoints) providerEndpoints {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.endpoints.Authorize == "" {
		s.endpoints = e
	}
	return s.endpoints
}

func randomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (s *OIDCSource) Begin(ctx context.Context, redirectURI, peer string) (string, error) {
	eps, err := s.resolveEndpoints(ctx)
	if err != nil {
		return "", err
	}
	state, err := randomURLSafe(32)
	if err != nil {
		return "", err
	}
	verifier, err := randomURLSafe(32)
	if err != nil {
		return "", err
	}
	if err := s.reg.putPending(ctx, state, verifier, redirectURI, peer); err != nil {
		return "", err
	}

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", s.cfg.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", strings.Join(s.cfg.scopeList(), " "))
	q.Set("state", state)
	q.Set("code_challenge", pkceChallenge(verifier))
	q.Set("code_challenge_method", "S256")

	sep := "?"
	if strings.Contains(eps.Authorize, "?") {
		sep = "&"
	}
	return eps.Authorize + sep + q.Encode(), nil
}

type tokenResponse struct {
	IDToken     string `json:"id_token"`
	AccessToken string `json:"access_token"`
}

func (s *OIDCSource) Complete(ctx context.Context, state, code string) (string, time.Duration, error) {
	verifier, redirectURI, ok, err := s.reg.takePending(ctx, state)
	if err != nil {
		return "", 0, err
	}
	if !ok {
		return "", 0, fmt.Errorf("%w: this sign-in link is unknown or has expired; start again", ErrAuthFlow)
	}
	eps, err := s.resolveEndpoints(ctx)
	if err != nil {
		return "", 0, err
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", s.cfg.ClientID)
	form.Set("client_secret", s.cfg.ClientSecret)
	form.Set("code_verifier", verifier)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, eps.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("%w: the identity provider could not be reached to exchange the code", ErrAuthFlow)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("%w: the identity provider refused the code exchange", ErrAuthFlow)
	}
	var tr tokenResponse
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&tr); err != nil {
		return "", 0, fmt.Errorf("%w: the identity provider's token response is not valid JSON", ErrAuthFlow)
	}

	sub, groups, err := s.claimsFrom(ctx, tr, eps)
	if err != nil {
		return "", 0, err
	}
	principal, err := QualifyOIDC(s.digest, sub)
	if err != nil {
		return "", 0, fmt.Errorf("%w: %s", ErrAuthFlow, err.Error())
	}
	maxGroups := s.cfg.MaxGroups
	if maxGroups <= 0 {
		maxGroups = DefaultMaxGroups
	}
	if len(groups) > maxGroups {
		return "", 0, fmt.Errorf("%w: the identity provider returned %d groups, more than this server accepts (%d); dropping some would silently discard a group that carries a grant, so the sign-in is refused — ask the administrator to raise -max-groups or have the provider send fewer", ErrAuthFlow, len(groups), maxGroups)
	}
	qualified := make([]string, 0, len(groups))
	for _, g := range groups {
		q, err := QualifyOIDCGroup(s.digest, g)
		if err != nil {
			return "", 0, fmt.Errorf("%w: %s", ErrAuthFlow, err.Error())
		}
		qualified = append(qualified, q)
	}

	ttl := s.cfg.TokenTTL
	if ttl == 0 {
		ttl = DefaultTokenTTL
	}
	ring, err := s.mintingRing(ctx)
	if err != nil {
		return "", 0, err
	}
	tok, err := MintToken(ring, principal, qualified, ttl, time.Now())
	if err != nil {
		return "", 0, err
	}
	return tok, ttl, nil
}

func (s *OIDCSource) claimsFrom(ctx context.Context, tr tokenResponse, eps providerEndpoints) (string, []string, error) {
	if tr.IDToken != "" {
		claims, err := unverifiedClaims(tr.IDToken)
		if err == nil {
			if err := s.checkIDTokenClaims(claims); err != nil {
				return "", nil, err
			}
			sub, _ := claims["sub"].(string)
			if sub != "" {
				return sub, groupClaims(claims, s.cfg.GroupsClaim), nil
			}
		}
	}
	if tr.AccessToken == "" || eps.UserInfo == "" {
		return "", nil, fmt.Errorf("%w: the identity provider returned no usable subject claim", ErrAuthFlow)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, eps.UserInfo, nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tr.AccessToken)
	req.Header.Set("Accept", "application/json")
	res, err := s.client.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("%w: the identity provider's user endpoint could not be reached", ErrAuthFlow)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("%w: the identity provider's user endpoint answered %d", ErrAuthFlow, res.StatusCode)
	}
	var claims map[string]any
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&claims); err != nil {
		return "", nil, fmt.Errorf("%w: the identity provider's user response is not valid JSON", ErrAuthFlow)
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		if id, ok := claims["id"].(float64); ok {
			sub = fmt.Sprintf("%d", int64(id))
		}
	}
	if sub == "" {
		return "", nil, fmt.Errorf("%w: the identity provider returned no usable subject claim", ErrAuthFlow)
	}
	return sub, groupClaims(claims, s.cfg.GroupsClaim), nil
}

func (s *OIDCSource) checkIDTokenClaims(claims map[string]any) error {
	if s.cfg.Preset == PresetGitHub {
		return nil
	}
	iss, _ := claims["iss"].(string)
	if iss != "" && strings.TrimRight(iss, "/") != strings.TrimRight(s.cfg.Issuer, "/") {
		return fmt.Errorf("%w: the identity provider returned a token issued by %q, not by the configured issuer; the sign-in is refused rather than trusting it", ErrAuthFlow, iss)
	}
	if !audienceMatches(claims["aud"], s.cfg.ClientID) {
		return fmt.Errorf("%w: the identity provider returned a token issued for a different application than this server's client id; the sign-in is refused rather than trusting it", ErrAuthFlow)
	}
	if exp, ok := claims["exp"].(float64); ok && int64(exp) <= time.Now().Unix() {
		return fmt.Errorf("%w: the identity provider returned an already-expired token; start the sign-in again", ErrAuthFlow)
	}
	return nil
}

func audienceMatches(raw any, clientID string) bool {
	switch aud := raw.(type) {
	case nil:
		return true
	case string:
		return aud == clientID
	case []any:
		for _, v := range aud {
			if s, ok := v.(string); ok && s == clientID {
				return true
			}
		}
		return false
	}
	return false
}

func unverifiedClaims(idToken string) (map[string]any, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("id token is not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil, err
	}
	return claims, nil
}

func groupClaims(claims map[string]any, key string) []string {
	if key == "" {
		key = "groups"
	}
	raw, ok := claims[key]
	if !ok {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, v := range list {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (s *OIDCSource) Rotate(ctx context.Context, retirePredecessors bool) (Keyring, error) {
	s.rotateMu.Lock()
	defer s.rotateMu.Unlock()
	cached, _ := s.currentRing()
	ring, err := s.reg.RotateSigningKey(ctx, cached.Deployment, retirePredecessors)
	if err != nil {
		return Keyring{}, err
	}
	return s.installRing(ring), nil
}

func (s *OIDCSource) currentRing() (Keyring, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ring, s.ringGen
}

func (s *OIDCSource) installRing(ring Keyring) Keyring {
	s.mu.Lock()
	s.ring, s.ringGen = ring, s.ringGen+1
	publish := s.onRing
	s.mu.Unlock()
	if publish != nil {
		publish(ring)
	}
	return ring
}

func (s *OIDCSource) installRingIfCurrent(ring Keyring, gen uint64) Keyring {
	s.mu.Lock()
	if s.ringGen != gen {
		held := s.ring
		s.mu.Unlock()
		return held
	}
	s.ring, s.ringGen = ring, s.ringGen+1
	publish := s.onRing
	s.mu.Unlock()
	if publish != nil {
		publish(ring)
	}
	return ring
}

func (s *OIDCSource) mintingRing(ctx context.Context) (Keyring, error) {
	cached, gen := s.currentRing()
	if s.reg == nil {
		return cached, nil
	}
	fresh, err := s.reg.LoadKeyring(ctx, cached.Deployment)
	if err != nil {
		return Keyring{}, err
	}
	return s.installRingIfCurrent(fresh, gen), nil
}
