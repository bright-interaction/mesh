package web

import (
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"strings"
)

// errRemoteNeedsToken is returned when the viewer is bound beyond loopback without
// a token. Fail closed: an exposed viewer must be authenticated.
var errRemoteNeedsToken = errors.New("refusing to bind beyond loopback without a token: pass --token <secret> (or MESH_UI_TOKEN), or bind 127.0.0.1")

// authConfig controls access to the viewer. The local viewer defaults to a
// loopback bind where only the local user can reach it, so no token is needed.
// Binding beyond loopback is fail-closed: it requires a token (mirrors the
// internal/sshserve model) so an exposed viewer (with editable settings) can never
// be reached unauthenticated.
type authConfig struct {
	token    string // bearer token; empty means "no auth" (loopback only)
	loopback bool   // whether the bind address is loopback-only
}

// newAuthConfig validates the bind/token combination, failing closed: a
// non-loopback bind without a token is refused at startup, not silently opened.
func newAuthConfig(addr, token string) (authConfig, error) {
	lo := isLoopbackAddr(addr)
	if !lo && token == "" {
		return authConfig{}, errRemoteNeedsToken
	}
	return authConfig{token: strings.TrimSpace(token), loopback: lo}, nil
}

// guard wraps a handler, enforcing the bearer token when one is configured. Static
// assets and the SPA shell stay open (they carry no vault data and the SPA needs to
// load before it can prompt for the token); the token gates the /api surface.
func (a authConfig) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.token == "" || !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if !a.tokenOK(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authRequired reports whether the SPA must present a token (used by the shell to
// decide whether to prompt). Surfaced at GET /api/whoami-style probes.
func (a authConfig) authRequired() bool { return a.token != "" }

func (a authConfig) tokenOK(r *http.Request) bool {
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
	if got == "" {
		got = r.URL.Query().Get("token") // allow ?token= for EventSource/links that cannot set headers
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) == 1
}

// isLoopbackAddr reports whether a host:port bind address is loopback-only. A bare
// ":7474" (all interfaces) or an explicit non-loopback host is NOT loopback.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return false // ":7474" binds all interfaces
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
