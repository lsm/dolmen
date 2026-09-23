package api

import (
	"net"
	"net/http"
	"strings"

	"github.com/lsm/dolmen/skill"
)

func ForwardingGuard(next http.Handler, trusted []*net.IPNet) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !peerTrusted(r, trusted) {
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

func peerTrusted(r *http.Request, trusted []*net.IPNet) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	for _, cidr := range trusted {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}
