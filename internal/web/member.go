package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
)

// memberAuth backs the web app with the hub's per-member client tokens, used when the
// app is served for a team via `mesh ui --hub-db`. Each member signs in with their own
// client token; the browser then carries a client-bound HMAC cookie. This replaces the
// single shared MESH_UI_TOKEN so the graph/search/note surfaces can be scoped to the
// signed-in member (via the Server's scopeResolver).
type memberAuth struct {
	// verify resolves a raw client token to its client id (ok=false for an unknown token).
	verify func(token string) (clientID int64, user string, ok bool)
	// scopesFor returns the member's readable scopes; nil = unrestricted (no scoping configured).
	scopesFor func(clientID int64) map[string]bool
	key       []byte // per-process HMAC key for the client-bound cookie
}

const memberCookie = "mesh_member"

func newMemberAuth(verify func(string) (int64, string, bool), scopesFor func(int64) map[string]bool) *memberAuth {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	return &memberAuth{verify: verify, scopesFor: scopesFor, key: key}
}

// sign returns the client-bound cookie value "<id>.<hmac>". A per-process key means
// cookies invalidate on restart (members re-sign-in), which is acceptable and avoids a
// server-side session store.
func (m *memberAuth) sign(id int64) string {
	ids := strconv.FormatInt(id, 10)
	mac := hmac.New(sha256.New, m.key)
	mac.Write([]byte("mesh-member-v1:" + ids))
	return ids + "." + hex.EncodeToString(mac.Sum(nil))
}

// clientFromCookie validates a cookie value (constant-time) and returns its client id.
func (m *memberAuth) clientFromCookie(v string) (int64, bool) {
	dot := strings.LastIndexByte(v, '.')
	if dot <= 0 {
		return 0, false
	}
	id, err := strconv.ParseInt(v[:dot], 10, 64)
	if err != nil {
		return 0, false
	}
	if subtle.ConstantTimeCompare([]byte(v), []byte(m.sign(id))) != 1 {
		return 0, false
	}
	return id, true
}

// clientFromRequest resolves the requesting member from the cookie, or a bearer /
// ?token client token. ok=false when unauthenticated.
func (m *memberAuth) clientFromRequest(r *http.Request) (int64, bool) {
	if c, err := r.Cookie(memberCookie); err == nil && c.Value != "" {
		if id, ok := m.clientFromCookie(c.Value); ok {
			return id, true
		}
	}
	// Bearer header only; the ?token= query fallback was removed so a member's client
	// token never lands in access/proxy logs, history, or a Referer header.
	tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
	if tok != "" {
		if id, _, ok := m.verify(tok); ok {
			return id, true
		}
	}
	return 0, false
}

// SetMemberAuth puts the web app in per-member mode: requests authenticate as a hub
// client and the graph/search/note surfaces are scoped to that member. verify checks a
// client token; scopesFor returns a member's readable scopes (nil = unrestricted).
func (s *Server) SetMemberAuth(verify func(token string) (int64, string, bool), scopesFor func(int64) map[string]bool) {
	s.member = newMemberAuth(verify, scopesFor)
	s.scopeResolver = func(r *http.Request) map[string]bool {
		id, ok := s.member.clientFromRequest(r)
		if !ok {
			return map[string]bool{} // deny-all for an unresolved request (the guard blocks these anyway)
		}
		return s.member.scopesFor(id) // nil here = unrestricted (scoping not configured)
	}
}

// memberGuard gates every non-open path on a resolved member, replacing the single
// shared-token guard when member mode is active.
func (s *Server) memberGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isOpenPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := s.member.clientFromRequest(r); !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
