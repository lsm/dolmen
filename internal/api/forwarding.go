package api

import (
	"log/slog"
	"net"
	"net/http"
	"sync"

	"github.com/lsm/dolmen/internal/auth"
	"github.com/lsm/dolmen/skill"
)

const droppedForwardingAdvice = "forwarding headers shape the public links dolmen advertises (skills, MCP, openapi.json) and are honored only from peers in DOLMEN_TRUSTED_PROXIES; if this peer is your reverse proxy, ingress or gateway, add its address range there, or set DOLMEN_BASE_URL to the full public URL"

func ForwardingGuard(next http.Handler, trusted []*net.IPNet) http.Handler {
	var warnOnce sync.Once
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !auth.PeerTrusted(r, trusted) {
			var strip []string
			for _, name := range skill.ForwardingHeaders {
				if r.Header.Get(name) != "" {
					strip = append(strip, name)
				}
			}
			if len(strip) > 0 {
				warnOnce.Do(func() {
					peer, _, err := net.SplitHostPort(r.RemoteAddr)
					if err != nil {
						peer = r.RemoteAddr
					}
					slog.Warn("dropped forwarding headers from a peer not in DOLMEN_TRUSTED_PROXIES",
						"peer", peer, "headers", strip, "advice", droppedForwardingAdvice)
				})
				r = r.Clone(r.Context())
				for _, name := range strip {
					r.Header.Del(name)
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
