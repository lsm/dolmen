package auth

import (
	"errors"
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

func TestGroupClaimShapes(t *testing.T) {
	for name, tc := range map[string]struct {
		claims map[string]any
		key    string
		want   []string
		bad    bool
	}{
		"absent":            {claims: map[string]any{"sub": "u"}},
		"explicit null":     {claims: map[string]any{"groups": nil}},
		"empty array":       {claims: map[string]any{"groups": []any{}}, want: []string{}},
		"names":             {claims: map[string]any{"groups": []any{"platform", "sre"}}, want: []string{"platform", "sre"}},
		"configured key":    {claims: map[string]any{"roles": []any{"platform"}}, key: "roles", want: []string{"platform"}},
		"string":            {claims: map[string]any{"groups": "platform sre"}, bad: true},
		"number":            {claims: map[string]any{"groups": float64(3)}, bad: true},
		"object":            {claims: map[string]any{"groups": map[string]any{"a": true}}, bad: true},
		"number inside":     {claims: map[string]any{"groups": []any{"platform", float64(3)}}, bad: true},
		"empty name inside": {claims: map[string]any{"groups": []any{"platform", ""}}, bad: true},
		"nested array":      {claims: map[string]any{"groups": []any{[]any{"platform"}}}, bad: true},
	} {
		got, err := groupClaims(tc.claims, tc.key)
		if tc.bad {
			if err == nil {
				t.Fatalf("%s: an unusable groups claim signed in with %v groups, so every grant on those groups silently misses", name, got)
			}
			if !errors.Is(err, ErrAuthFlow) {
				t.Fatalf("%s: the refusal is not a sign-in flow error: %v", name, err)
			}
			if !strings.Contains(err.Error(), "DOLMEN_AUTH_OIDC_GROUPS_CLAIM") && !strings.Contains(err.Error(), "discard") {
				t.Fatalf("%s: the refusal names no remediation: %v", name, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: rejected a usable claim: %v", name, err)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("%s: got %v, want %v", name, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: got %v, want %v", name, got, tc.want)
			}
		}
	}
}
