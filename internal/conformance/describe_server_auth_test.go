package conformance

import (
	"reflect"
	"testing"
)

func TestDescribeServerTellsAClientHowItCanAuthenticate(t *testing.T) {
	cases := []struct {
		name string
		h    func(t *testing.T) *harness
		want map[string]any
	}{
		{"admin key only", func(t *testing.T) *harness { return newHarnessMode(t, authAdminKey) },
			map[string]any{"sign_in": false, "identity_headers": false}},
		{"behind a gateway", func(t *testing.T) *harness { return newHarnessMode(t, authGateway) },
			map[string]any{"sign_in": false, "identity_headers": true}},
		{"with sign-in", func(t *testing.T) *harness { return oidcHarness(t, newIssuerStub(t, "00u1a2b3", nil)) },
			map[string]any{"sign_in": true, "identity_headers": false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.h(t)
			data := h.mustHTTP("describe_server", map[string]any{})
			if got, _ := data["auth"].(map[string]any); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("describe_server auth = %v, want %v", data["auth"], tc.want)
			}
		})
	}
}

func TestDescribeServerSaysNothingAboutAuthWhenItIsOff(t *testing.T) {
	h := newHarnessMode(t, authOff)
	if data := h.mustHTTP("describe_server", map[string]any{}); data["auth"] != nil {
		t.Fatalf("with auth off describe_server must keep its v0.2 shape, got auth = %v", data["auth"])
	}
}
