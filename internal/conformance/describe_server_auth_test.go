package conformance

import (
	"reflect"
	"testing"
)

func TestDescribeServerTellsAClientHowItCanAuthenticate(t *testing.T) {
	cases := []struct {
		name string
		h    func(t *testing.T) *harness
		want []any
	}{
		{"admin key only", func(t *testing.T) *harness { return newHarnessMode(t, authAdminKey) },
			[]any{"admin-key", "api-keys"}},
		{"behind a gateway", func(t *testing.T) *harness { return newHarnessMode(t, authGateway) },
			[]any{"admin-key", "api-keys", "trusted-proxy"}},
		{"with sign-in", func(t *testing.T) *harness { return oidcHarness(t, newIssuerStub(t, "00u1a2b3", nil)) },
			[]any{"admin-key", "api-keys", "oidc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.h(t)
			data := h.mustHTTP("describe_server", map[string]any{})
			got, _ := data["auth"].(map[string]any)
			if got["mode"] != "on" || !reflect.DeepEqual(got["sources"], tc.want) {
				t.Fatalf("describe_server auth = %v, want mode on and sources %v", data["auth"], tc.want)
			}
			caps, _ := data["capabilities"].(map[string]any)
			if caps["query_dialect"] == nil || caps["vector_execution"] == nil {
				t.Fatalf("describe_server must inline the engine capabilities under auth on: %v", data["capabilities"])
			}
		})
	}
}

func TestDescribeServerSaysNothingAboutAuthWhenItIsOff(t *testing.T) {
	h := newHarnessMode(t, authOff)
	if data := h.mustHTTP("describe_server", map[string]any{}); data["auth"] != nil || data["capabilities"] != nil {
		t.Fatalf("with auth off describe_server must keep its v0.2 shape, got %v", data)
	}
}
