package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"
)

// sessionCookie is the HttpOnly cookie set by POST /api/login. Its value is the
// HMAC-derived session value (never the raw token), so a browser stays signed in
// without the SPA ever holding the secret.
const sessionCookie = "mesh_session"

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

// guard wraps a handler, enforcing the bearer token when one is configured. Only
// the SPA shell and static assets stay open (they carry no vault data and must load
// so the SPA can prompt for the token); everything else, including the graph payload
// and every /api route, is gated. So an exposed viewer never leaks vault data.
func (a authConfig) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.token == "" || isOpenPath(r.URL.Path) {
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

// isOpenPath is the unauthenticated allowlist: the shell, its assets, and the
// login/logout endpoints (which must be reachable to obtain or drop a session).
func isOpenPath(p string) bool {
	return p == "/" || strings.HasPrefix(p, "/assets/") || p == "/api/login" || p == "/api/logout"
}

// sessionValue derives the opaque session-cookie value from the token via HMAC, so
// the raw token is never stored in the browser. Deterministic, so no server-side
// session store is needed; constant-time compared on every request.
func (a authConfig) sessionValue() string {
	mac := hmac.New(sha256.New, []byte(a.token))
	mac.Write([]byte("mesh-ui-session-v1"))
	return hex.EncodeToString(mac.Sum(nil))
}

// authRequired reports whether the SPA must present a token (used by the shell to
// decide whether to prompt). Surfaced at GET /api/whoami-style probes.
func (a authConfig) authRequired() bool { return a.token != "" }

func (a authConfig) tokenOK(r *http.Request) bool {
	// Session cookie (set by POST /api/login) is the primary browser path: HttpOnly,
	// so it survives a tab close and is never exposed to JS.
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if subtle.ConstantTimeCompare([]byte(c.Value), []byte(a.sessionValue())) == 1 {
			return true
		}
	}
	// Bearer header (CLI). The ?token= query fallback was removed: a secret in the URL
	// leaks to access/proxy logs, history, and the Referer header. The SPA uses the
	// cookie; the CLI uses the Authorization header.
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) == 1
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
