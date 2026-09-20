package auth

import (
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"strings"
)

const (
	OIDCSourceName    = "oidc"
	OIDCQualifierTag  = "v1"
	OIDCDigestLen     = 26
	oidcQualifyPrefix = "oidc:" + OIDCQualifierTag + ":"
)

func IssuerDigest(issuer string) string {
	sum := sha256.Sum256([]byte(issuer))
	enc := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:]))
	return enc[:OIDCDigestLen]
}

func QualifyOIDC(issuerDigest, claim string) (string, error) {
	if claim == "" {
		return "", fmt.Errorf("the identity provider returned an empty claim, so there is nothing to authenticate as")
	}
	qualified := oidcQualifyPrefix + issuerDigest + ":" + claim
	if !principalRe.MatchString(qualified) {
		return "", fmt.Errorf("the identity provider's claim does not fit dolmen's identity shape once qualified by issuer: it must stay within 256 printable ASCII characters with no space, and a grant could never name it otherwise")
	}
	if qualified == AdminPrincipal {
		return "", fmt.Errorf("the qualified identity collides with the reserved bootstrap principal %q", AdminPrincipal)
	}
	return qualified, nil
}

func QualifyOIDCGroup(issuerDigest, claim string) (string, error) {
	if claim == "" {
		return "", fmt.Errorf("the identity provider returned an empty group claim")
	}
	qualified := oidcQualifyPrefix + issuerDigest + ":" + claim
	if !groupRe.MatchString(qualified) {
		return "", fmt.Errorf("group %q does not fit dolmen's group shape once qualified by issuer: it must stay within 128 printable ASCII characters with no space or comma", claim)
	}
	return qualified, nil
}

func IsOIDCQualified(id string) bool {
	return strings.HasPrefix(id, oidcQualifyPrefix)
}

func OIDCIssuerOf(id string) (string, bool) {
	rest, ok := strings.CutPrefix(id, oidcQualifyPrefix)
	if !ok {
		return "", false
	}
	digest, _, ok := strings.Cut(rest, ":")
	if !ok || len(digest) != OIDCDigestLen {
		return "", false
	}
	return digest, true
}
