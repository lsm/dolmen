package auth

import (
	"strings"
	"testing"
)

func validOIDC() OIDCConfig {
	return OIDCConfig{
		Issuer:       "https://login.example.com/tenant/v2.0",
		ClientID:     "client",
		ClientSecret: "secret",
	}
}

func TestPresetAndIssuerTogetherAreRefused(t *testing.T) {
	c := validOIDC()
	c.Preset = PresetGitHub
	err := c.Validate()
	if err == nil {
		t.Fatal("a preset alongside an issuer was accepted, so the issuer is silently ignored and grants naming it never match")
	}
	for _, want := range []string{"DOLMEN_AUTH_OIDC_PRESET", "DOLMEN_AUTH_OIDC_ISSUER", c.IssuerKey()} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error does not name %q: %v", want, err)
		}
	}
}

func TestPresetAloneAndIssuerAloneValidate(t *testing.T) {
	c := validOIDC()
	if err := c.Validate(); err != nil {
		t.Fatalf("issuer alone rejected: %v", err)
	}
	c = OIDCConfig{Preset: PresetGitHub, ClientID: "client", ClientSecret: "secret"}
	if err := c.Validate(); err != nil {
		t.Fatalf("preset alone rejected: %v", err)
	}
	if c.IssuerKey() == "" {
		t.Fatal("the github preset pins no issuer key")
	}
}

func TestDisabledConfigValidates(t *testing.T) {
	var c OIDCConfig
	if c.Enabled() {
		t.Fatal("an empty config reports enabled")
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("a disabled config was rejected: %v", err)
	}
}
