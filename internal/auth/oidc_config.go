package auth

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	Scopes       []string
	GroupsClaim  string
	TokenTTL     time.Duration
	DeploymentID string
	Preset       string
	MaxGroups    int
}

const (
	PresetGitHub = "github"

	githubAuthorizeURL = "https://github.com/login/oauth/authorize"
	githubTokenURL     = "https://github.com/login/oauth/access_token"
	githubUserURL      = "https://api.github.com/user"
)

func (c OIDCConfig) Enabled() bool { return c.Issuer != "" || c.Preset != "" }

func (c *OIDCConfig) Validate() error {
	if !c.Enabled() {
		return nil
	}
	if c.Preset != "" && c.Preset != PresetGitHub {
		return fmt.Errorf("unknown OIDC preset %q: the only preset is %q; otherwise set DOLMEN_AUTH_OIDC_ISSUER to a generic OIDC issuer URL", c.Preset, PresetGitHub)
	}
	if c.Preset == "" {
		u, err := url.Parse(c.Issuer)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("DOLMEN_AUTH_OIDC_ISSUER must be an https URL naming the identity provider, for example https://login.microsoftonline.com/<tenant>/v2.0")
		}
		if u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("DOLMEN_AUTH_OIDC_ISSUER must be the bare issuer URL, with no query or fragment")
		}
	}
	if c.ClientID == "" {
		return fmt.Errorf("DOLMEN_AUTH_OIDC_CLIENT_ID is required when the OIDC source is enabled")
	}
	if c.ClientSecret == "" {
		return fmt.Errorf("DOLMEN_AUTH_OIDC_CLIENT_SECRET is required when the OIDC source is enabled (environment only, never a flag)")
	}
	if c.TokenTTL == 0 {
		c.TokenTTL = DefaultTokenTTL
	}
	if err := ValidateTokenTTL(c.TokenTTL); err != nil {
		return fmt.Errorf("DOLMEN_AUTH_OIDC_TOKEN_TTL: %w", err)
	}
	for _, s := range c.Scopes {
		if strings.ContainsAny(s, " \t\n") {
			return fmt.Errorf("OIDC scope %q must not contain whitespace; separate scopes with commas", s)
		}
	}
	return nil
}

func (c OIDCConfig) IssuerKey() string {
	if c.Preset == PresetGitHub {
		return "https://github.com/login/oauth"
	}
	return c.Issuer
}

func (c OIDCConfig) scopeList() []string {
	if c.Preset == PresetGitHub {
		base := []string{"read:user"}
		return append(base, c.Scopes...)
	}
	base := []string{"openid", "profile"}
	return append(base, c.Scopes...)
}

func ParseScopes(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var out []string
	for _, s := range strings.Split(raw, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
