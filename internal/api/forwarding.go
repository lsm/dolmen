package api

import (
	"net"
	"net/http"

	"github.com/lsm/dolmen/internal/auth"
	"github.com/lsm/dolmen/skill"
)

func ForwardingGuard(next http.Handler, trusted []*net.IPNet) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !auth.PeerTrusted(r, trusted) {
			var strip []string
			for _, name := range skill.ForwardingHeaders {
				if r.Header.Get(name) != "" {
					strip = append(strip, name)
				}
			}
			if len(strip) > 0 {
				r = r.Clone(r.Context())
				for _, name := range strip {
					r.Header.Del(name)
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
