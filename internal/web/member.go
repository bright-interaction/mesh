// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net/http"
	"os"
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
	// roleFor returns the member's role and whether the client still exists. ok=false
	// means the client was removed/revoked, so a still-valid cookie must stop working
	// immediately instead of at cookie expiry.
	roleFor func(clientID int64) (role string, ok bool)
	key     []byte // per-process HMAC key for the client-bound cookie
}

const memberCookie = "mesh_member"

func newMemberAuth(verify func(string) (int64, string, bool), scopesFor func(int64) map[string]bool, roleFor func(int64) (string, bool)) *memberAuth {
	return &memberAuth{verify: verify, scopesFor: scopesFor, roleFor: roleFor, key: stableMemberKey()}
}

// hub role order (owner > admin > member > viewer). Duplicated minimally here because
// internal/web is the open core and must not import the pro internal/hub package; the
// pro side hands us the role string via roleFor and we only need to rank it.
func roleRank(role string) int {
	switch strings.TrimSpace(role) {
	case "owner":
		return 4
	case "admin":
		return 3
	case "member":
		return 2
	case "viewer":
		return 1
	}
	return 0
}

// stableMemberKey derives the cookie-signing key from a STABLE server secret so a
// signed-in member survives a server restart/redeploy instead of being logged out
// every time (the 30-day cookie then actually lasts 30 days). It is HMAC-derived and
// domain-separated, so the secret itself is never exposed and a member cookie cannot be
// forged without it. Priority: MESH_UI_COOKIE_SECRET, then MESH_UI_TOKEN (set on a
// hosted hub). With neither, it falls back to a random per-process key (sessions then
// end on restart) and logs how to make them persist.
func stableMemberKey() []byte {
	for _, env := range []string{"MESH_UI_COOKIE_SECRET", "MESH_UI_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			mac := hmac.New(sha256.New, []byte(v))
			mac.Write([]byte("mesh-ui-member-cookie-v1"))
			return mac.Sum(nil)
		}
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	slog.Warn("mesh ui: member cookies use an ephemeral signing key, so members re-login after every restart; set MESH_UI_COOKIE_SECRET (or MESH_UI_TOKEN) to keep them signed in")
	return key
}

// sign returns the client-bound cookie value "<id>.<hmac>", signed with the stable key
// so it stays valid across restarts.
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

// member resolves the requesting member from the cookie, or a bearer client token,
// and returns their id + role. It re-checks that the client still exists (roleFor) on
// every request, so a removed member's still-valid cookie stops working immediately
// instead of lingering until the 30-day cookie expires. ok=false when unauthenticated
// or when the client has been revoked.
func (m *memberAuth) member(r *http.Request) (id int64, role string, ok bool) {
	if c, err := r.Cookie(memberCookie); err == nil && c.Value != "" {
		if cid, valid := m.clientFromCookie(c.Value); valid {
			if role, exists := m.roleFor(cid); exists {
				return cid, role, true
			}
			return 0, "", false // cookie signature valid but the client was revoked
		}
	}
	// Bearer header only; the ?token= query fallback was removed so a member's client
	// token never lands in access/proxy logs, history, or a Referer header.
	tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
	if tok != "" {
		if cid, _, valid := m.verify(tok); valid {
			if role, exists := m.roleFor(cid); exists {
				return cid, role, true
			}
		}
	}
	return 0, "", false
}

// clientFromRequest resolves the requesting member id (existence-checked). ok=false
// when unauthenticated or revoked.
func (m *memberAuth) clientFromRequest(r *http.Request) (int64, bool) {
	id, _, ok := m.member(r)
	return id, ok
}

// SetMemberAuth puts the web app in per-member mode: requests authenticate as a hub
// client and the graph/search/note surfaces are scoped to that member. verify checks a
// client token; scopesFor returns a member's readable scopes (nil = unrestricted);
// roleFor returns the member's role and whether the client still exists.
func (s *Server) SetMemberAuth(verify func(token string) (int64, string, bool), scopesFor func(int64) map[string]bool, roleFor func(int64) (string, bool)) {
	s.member = newMemberAuth(verify, scopesFor, roleFor)
	s.scopeResolver = func(r *http.Request) map[string]bool {
		id, ok := s.member.clientFromRequest(r)
		if !ok {
			return map[string]bool{} // deny-all for an unresolved request (the guard blocks these anyway)
		}
		return s.member.scopesFor(id) // nil here = unrestricted (scoping not configured)
	}
}

// requireAdmin gates a state-changing handler on an admin-or-higher role. In standalone
// mode (no member auth) the single trusted token already gates every route, so it is a
// no-op. In member mode a viewer/member is refused: config edits, reindex, and the
// pending review-queue promote/discard are admin actions, mirroring the hub's own
// admin-gated Integrations config. Returns false (and writes 403) when refused.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if s.member == nil {
		return true // standalone single-token viewer: the token is the gate
	}
	_, role, ok := s.member.member(r)
	if !ok || roleRank(role) < roleRank("admin") {
		http.Error(w, "forbidden: an admin role is required for this action", http.StatusForbidden)
		return false
	}
	return true
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
