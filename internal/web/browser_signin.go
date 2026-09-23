// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"errors"
	"html"
	"net/http"
	"net/url"
	"strings"
)

// BrowserSignIn is the commercial host's explicit bridge to its existing browser
// identity. Resolve must verify only a browser cookie, never headers, query claims
// or delegated access tokens. The viewer still rechecks its own current role/ACLs.
type BrowserSignIn struct {
	LoginURL string
	Resolve  func(*http.Request) (id int64, account string, ok bool)
	Clear    func(http.ResponseWriter)
}

func (s *Server) SetBrowserSignIn(b BrowserSignIn) error {
	if s.member == nil || s.connections == nil || b.Resolve == nil || b.Clear == nil {
		return errors.New("browser sign-in requires member auth, connections and a cookie resolver")
	}
	u, err := url.Parse(b.LoginURL)
	if err != nil || u.Scheme+"://"+u.Host != s.connections.origin || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "/auth/oidc/login" || u.RawPath != "" {
		return errors.New("MESH_UI_HUB_SIGN_IN_URL must be the same-origin HTTPS /auth/oidc/login endpoint")
	}
	s.member.browser = &b
	return nil
}

func (s *Server) browserAccount(r *http.Request) (int64, string, bool) {
	if s.member == nil || s.member.browser == nil {
		return 0, "", false
	}
	id, account, ok := s.member.browser.Resolve(r)
	if !ok || id <= 0 {
		return 0, "", false
	}
	role, _, exists := s.member.roleFor(id)
	return id, strings.TrimSpace(account), exists && roleRank(role) > 0
}

func (s *Server) browserSignInURL(r *http.Request) string {
	if s.member == nil || s.member.browser == nil {
		return ""
	}
	u, _ := url.Parse(s.member.browser.LoginURL) // validated at startup
	returnTo := s.basePath + "/connect"
	if r.URL.Path == "/" {
		returnTo = s.basePath + "/"
	} else if code := r.URL.Query().Get("user_code"); code != "" {
		returnTo += "?user_code=" + url.QueryEscape(code)
	}
	u.RawQuery = url.Values{"return_to": {returnTo}}.Encode()
	return u.String()
}

func (s *Server) browserSignInAttribute(r *http.Request) string {
	return html.EscapeString(s.browserSignInURL(r))
}
