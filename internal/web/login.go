// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

// handleLogin validates the access key and, on success, sets an HttpOnly session
// cookie so the browser stays signed in without the SPA ever holding the token. The
// cookie value is the HMAC-derived session value (never the raw key). This is an open
// path (reachable unauthenticated) and validates the key itself, constant-time.
// Accepts the key as JSON {"key":"..."} or a "key" form field.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	// Member mode (mesh ui --hub-db): the key is a hub client token; on success set a
	// client-bound cookie so the member's view is scoped to them.
	if s.member != nil {
		key := loginKey(w, r)
		id, _, ok := s.member.verify(key)
		if !ok {
			http.Error(w, "invalid access key", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     memberCookie,
			Value:    s.member.sign(id),
			Path:     s.cookiePath(),
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteStrictMode,
			MaxAge:   60 * 60 * 24 * 30, // 30 days
		})
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !s.auth.authRequired() {
		// No auth configured (loopback bind): nothing to sign into.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	key := loginKey(w, r)
	if subtle.ConstantTimeCompare([]byte(key), []byte(s.auth.token)) != 1 {
		http.Error(w, "invalid access key", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    s.auth.sessionValue(),
		Path:     s.cookiePath(),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   60 * 60 * 24 * 30, // 30 days
	})
	w.WriteHeader(http.StatusNoContent)
}

// handleLogout clears the session cookie (both the shared-token and member cookies).
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	name := sessionCookie
	if s.member != nil {
		name = memberCookie
	}
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     s.cookiePath(),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
	w.WriteHeader(http.StatusNoContent)
}

// loginKey reads the access key from a JSON body or a form field, capping the body.
func loginKey(w http.ResponseWriter, r *http.Request) string {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			Key string `json:"key"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		return strings.TrimSpace(body.Key)
	}
	return strings.TrimSpace(r.FormValue("key"))
}

// cookiePath scopes the session cookie to the app's base path, so it is sent with
// every /api request under the app but not leaked to other apps on the same host.
func (s *Server) cookiePath() string {
	if s.basePath == "" {
		return "/"
	}
	return s.basePath
}
