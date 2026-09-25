package api

import (
	"testing"

	"github.com/lsm/dolmen/internal/auth"
)

func TestEveryOperationHasAnAuthorizationRule(t *testing.T) {
	for name := range Ops {
		if _, ok := authRules[name]; !ok {
			t.Fatalf("operation %q has no authRules entry, so every auth:on call to it would answer 500", name)
		}
	}
	for name := range authOps {
		if _, ok := authRules[name]; !ok {
			t.Fatalf("auth-only operation %q has no authRules entry", name)
		}
	}
	for name := range authRules {
		_, base := Ops[name]
		_, only := authOps[name]
		if !base && !only {
			t.Fatalf("authRules names %q, which is not an operation", name)
		}
	}
}

func TestAuthorizationRulesNameRealVerbs(t *testing.T) {
	for name, rule := range authRules {
		if rule.Scope == scopeNone {
			if len(rule.Verbs) != 0 {
				t.Fatalf("operation %q needs no grant but lists verbs %v", name, rule.Verbs)
			}
			continue
		}
		if len(rule.Verbs) == 0 {
			t.Fatalf("operation %q is grant-protected but requires no verb", name)
		}
		for _, v := range rule.Verbs {
			if _, err := auth.ParseVerb(string(v)); err != nil {
				t.Fatalf("operation %q requires unknown verb %q", name, v)
			}
		}
	}
}

func TestEveryRevealInputIsGatedByTheRevealVerb(t *testing.T) {
	for name, def := range Ops {
		props, _ := def.InputSchema["properties"].(map[string]any)
		_, takesReveal := props["reveal"]
		if takesReveal != authRules[name].Reveal {
			t.Fatalf("operation %q: input takes reveal = %v but its authRules Reveal = %v, so a reveal could skip the reveal verb check", name, takesReveal, authRules[name].Reveal)
		}
	}
}

func TestAdminDoesNotImplyReveal(t *testing.T) {
	if auth.NewVerbSet(auth.VerbAdmin).Has(auth.VerbReveal) {
		t.Fatal("admin must not imply reveal")
	}
	if _, err := auth.ParseVerb("reveal"); err != nil {
		t.Fatal(err)
	}
}
