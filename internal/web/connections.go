// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/bright-interaction/mesh/internal/connectauth"
)

type connectionService struct {
	store                    *connectauth.Store
	publicURL, origin, host  string
	global, requests, starts *rateLimiter
}

type connectionContextKey struct{}
type connectionIdentity struct {
	subject, role string
	id, created   int64
	shared        bool
}

// EnableConnections must run before Handler/Serve. An explicit HTTPS audience
// prevents untrusted Host/Forwarded headers from selecting where a user approves
// or where a client sends credentials. Existing authentication is required.
func (s *Server) EnableConnections(publicURL, statePath string) error {
	if s.connections != nil {
		return errors.New("connections already configured")
	}
	u, err := url.Parse(publicURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.ForceQuery || strings.ContainsAny(publicURL, "\\ \t\n\r") || u.EscapedPath() != u.Path {
		return errors.New("MESH_UI_PUBLIC_URL must be an HTTPS viewer URL without credentials, query or fragment")
	}
	if strings.TrimSuffix(u.Path, "/") != s.basePath {
		return errors.New("MESH_UI_PUBLIC_URL path must match the viewer base path")
	}
	if s.member == nil && s.auth.token == "" {
		return errors.New("browser connections require authenticated Mesh accounts or a configured viewer token")
	}
	store, err := connectauth.Open(statePath)
	if err != nil {
		return err
	}
	u.Path = s.basePath
	s.connections = &connectionService{store: store, publicURL: u.String(), origin: u.Scheme + "://" + u.Host, host: u.Host,
		global: newRateLimiter(20, 40), requests: newRateLimiter(2, 20), starts: newRateLimiter(1.0/60, 5)}
	return nil
}

func connectionSharedIdentity(r *http.Request) bool {
	i, ok := r.Context().Value(connectionContextKey{}).(connectionIdentity)
	return ok && i.shared
}

func (s *Server) sharedConnectionSubject(prefix string) string {
	// Do not persist sessionValue: that HMAC is itself a browser credential.
	h := sha256.Sum256([]byte("mesh-connection-subject-v1:" + s.auth.token))
	return prefix + hex.EncodeToString(h[:])
}

func (s *Server) resolveConnectionSubject(subject string) (connectionIdentity, bool) {
	if s.member == nil {
		ok := s.auth.token != "" && subject == s.sharedConnectionSubject("standalone:")
		return connectionIdentity{subject: subject, shared: true, role: "admin"}, ok
	}
	if strings.HasPrefix(subject, "breakglass:") {
		role, created, ok := s.member.roleFor(-1)
		return connectionIdentity{subject: subject, id: -1, role: role, created: created}, ok && roleRank(role) > 0 && s.auth.token != "" && subject == s.sharedConnectionSubject("breakglass:")
	}
	parts := strings.Split(subject, ":")
	if len(parts) != 3 || parts[0] != "member" {
		return connectionIdentity{}, false
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id <= 0 {
		return connectionIdentity{}, false
	}
	generation, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return connectionIdentity{}, false
	}
	role, created, ok := s.member.roleFor(id)
	return connectionIdentity{subject: subject, id: id, created: created, role: role}, ok && generation == created && roleRank(role) > 0
}

// Browser consent/management accepts only an existing browser session, never a
// delegated bearer token that could silently mint further connections.
func (s *Server) connectionBrowser(r *http.Request) (connectionIdentity, bool) {
	clone := r.Clone(context.Background())
	clone.Header.Del("Authorization")
	if s.member != nil {
		id, _, ok := s.member.member(clone)
		if !ok {
			return connectionIdentity{}, false
		}
		if id < 0 {
			return s.resolveConnectionSubject(s.sharedConnectionSubject("breakglass:"))
		}
		_, created, exists := s.member.roleFor(id)
		if !exists {
			return connectionIdentity{}, false
		}
		return s.resolveConnectionSubject(fmt.Sprintf("member:%d:%d", id, created))
	}
	if _, err := r.Cookie(sessionCookie); err != nil || s.auth.token == "" || !s.auth.tokenOK(clone) {
		return connectionIdentity{}, false
	}
	return s.resolveConnectionSubject(s.sharedConnectionSubject("standalone:"))
}

func (s *Server) connectionGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := s.connections
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		connectionRequest := r.URL.Path == "/connect" || strings.HasPrefix(r.URL.Path, "/api/connect/") || strings.HasPrefix(token, "mesh_access_")
		if c != nil && connectionRequest && (r.Host != c.host || (r.Header.Get("Origin") != "" && r.Header.Get("Origin") != c.origin)) {
			connectionJSON(w, http.StatusForbidden, map[string]string{"error": "origin_not_allowed"})
			return
		}
		if c != nil && strings.HasPrefix(r.URL.Path, "/api/connect/") {
			if !c.global.allow("all") || !c.requests.allow(peerKey(r)) {
				w.Header().Set("Retry-After", "5")
				connectionJSON(w, 429, map[string]string{"error": "slow_down"})
				return
			}
		}
		if strings.HasPrefix(token, "mesh_access_") {
			if c == nil {
				connectionJSON(w, 401, map[string]string{"error": "invalid_token"})
				return
			}
			g, err := c.store.LookupAccess(r.Context(), token, c.publicURL)
			identity, ok := s.resolveConnectionSubject(g.Subject)
			if err != nil || !ok {
				connectionJSON(w, 401, map[string]string{"error": "invalid_token"})
				return
			}
			if g.Scope == connectauth.ScopeRead && r.Method != http.MethodGet && r.Method != http.MethodHead {
				connectionJSON(w, 403, map[string]string{"error": "insufficient_scope"})
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), connectionContextKey{}, identity))
		}
		next.ServeHTTP(w, r)
	})
}

func connectionJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func connectionError(w http.ResponseWriter, err error) {
	status, code := http.StatusBadRequest, "invalid_request"
	switch {
	case errors.Is(err, connectauth.ErrGrant):
		code = "invalid_grant"
	case errors.Is(err, connectauth.ErrPending):
		code = "authorization_pending"
	case errors.Is(err, connectauth.ErrSlowDown):
		code = "slow_down"
	case errors.Is(err, connectauth.ErrDenied):
		code = "access_denied"
	case errors.Is(err, connectauth.ErrExpired):
		code = "expired_token"
	case errors.Is(err, connectauth.ErrCapacity):
		status = 503
		code = "temporarily_unavailable"
	case !errors.Is(err, connectauth.ErrInvalid):
		status = 500
		code = "server_error"
	}
	connectionJSON(w, status, map[string]string{"error": code})
}

type connectionInput struct {
	ClientID     string `json:"client_id"`
	Scope        string `json:"scope"`
	DeviceCode   string `json:"device_code"`
	GrantType    string `json:"grant_type"`
	RefreshToken string `json:"refresh_token"`
	Token        string `json:"token"`
	UserCode     string `json:"user_code"`
	Decision     string `json:"decision"`
	GrantID      string `json:"grant_id"`
}

func connectionBody(w http.ResponseWriter, r *http.Request) (connectionInput, bool) {
	var in connectionInput
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		connectionError(w, connectauth.ErrInvalid)
		return in, false
	}
	if kind == "application/json" {
		d := json.NewDecoder(r.Body)
		d.DisallowUnknownFields()
		err = d.Decode(&in)
		if err == nil {
			var extra any
			if e := d.Decode(&extra); e != io.EOF {
				err = connectauth.ErrInvalid
			}
		}
	} else if kind == "application/x-www-form-urlencoded" {
		err = r.ParseForm()
		if err == nil {
			values := map[string]*string{"client_id": &in.ClientID, "scope": &in.Scope, "device_code": &in.DeviceCode, "grant_type": &in.GrantType, "refresh_token": &in.RefreshToken, "token": &in.Token, "user_code": &in.UserCode, "decision": &in.Decision, "grant_id": &in.GrantID}
			for key, valuesIn := range r.PostForm {
				dest, ok := values[key]
				if !ok || len(valuesIn) != 1 {
					err = connectauth.ErrInvalid
					break
				}
				*dest = valuesIn[0]
			}
		}
	} else {
		err = connectauth.ErrInvalid
	}
	if err != nil {
		connectionError(w, connectauth.ErrInvalid)
		return in, false
	}
	return in, true
}

func (s *Server) connectionEnabled(w http.ResponseWriter) bool {
	if s.connections == nil {
		connectionJSON(w, 404, map[string]string{"error": "connections_not_configured"})
		return false
	}
	return true
}

func (s *Server) handleConnectDevice(w http.ResponseWriter, r *http.Request) {
	if !s.connectionEnabled(w) {
		return
	}
	if !s.connections.starts.allow(peerKey(r)) {
		connectionJSON(w, 429, map[string]string{"error": "slow_down"})
		return
	}
	in, ok := connectionBody(w, r)
	if !ok {
		return
	}
	d, err := s.connections.store.Begin(r.Context(), in.ClientID, in.Scope, s.connections.publicURL)
	if err != nil {
		connectionError(w, err)
		return
	}
	uri := s.connections.publicURL + "/connect"
	connectionJSON(w, 200, struct {
		connectauth.Device
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
	}{d, uri, uri + "?user_code=" + url.QueryEscape(d.UserCode)})
}

func (s *Server) handleConnectToken(w http.ResponseWriter, r *http.Request) {
	if !s.connectionEnabled(w) {
		return
	}
	in, ok := connectionBody(w, r)
	if !ok {
		return
	}
	c := s.connections
	var tokens connectauth.Tokens
	var err error
	switch in.GrantType {
	case "urn:ietf:params:oauth:grant-type:device_code":
		tokens, err = c.store.Poll(r.Context(), in.DeviceCode, in.ClientID, c.publicURL)
		if err == nil {
			g, e := c.store.LookupAccess(r.Context(), tokens.AccessToken, c.publicURL)
			if _, valid := s.resolveConnectionSubject(g.Subject); e != nil || !valid {
				_ = c.store.RevokeToken(r.Context(), tokens.AccessToken, in.ClientID, c.publicURL)
				err = connectauth.ErrGrant
			}
		}
	case "refresh_token":
		var g connectauth.Grant
		g, err = c.store.LookupRefresh(r.Context(), in.RefreshToken, in.ClientID, c.publicURL)
		if err == nil {
			if _, valid := s.resolveConnectionSubject(g.Subject); !valid {
				_ = c.store.RevokeToken(r.Context(), in.RefreshToken, in.ClientID, c.publicURL)
				err = connectauth.ErrGrant
			} else {
				tokens, err = c.store.Refresh(r.Context(), in.RefreshToken, in.ClientID, c.publicURL)
			}
		}
	default:
		err = connectauth.ErrInvalid
	}
	if err != nil {
		connectionError(w, err)
		return
	}
	connectionJSON(w, 200, tokens)
}

func (s *Server) handleConnectCancel(w http.ResponseWriter, r *http.Request) {
	if !s.connectionEnabled(w) {
		return
	}
	in, ok := connectionBody(w, r)
	if !ok {
		return
	}
	if err := s.connections.store.Cancel(r.Context(), in.DeviceCode, in.ClientID, s.connections.publicURL); err != nil {
		connectionError(w, err)
		return
	}
	connectionJSON(w, 200, map[string]bool{"cancelled": true})
}

func (s *Server) handleConnectRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.connectionEnabled(w) {
		return
	}
	in, ok := connectionBody(w, r)
	if !ok {
		return
	}
	if err := s.connections.store.RevokeToken(r.Context(), in.Token, in.ClientID, s.connections.publicURL); err != nil {
		connectionError(w, err)
		return
	}
	connectionJSON(w, 200, map[string]bool{"revoked": true})
}

func (s *Server) requireConnectionBrowser(w http.ResponseWriter, r *http.Request) (connectionIdentity, bool) {
	if !s.connectionEnabled(w) {
		return connectionIdentity{}, false
	}
	// A mandatory exact Origin protects all cookie-authenticated mutations. Do not
	// infer it from Forwarded headers, Referer, or a missing browser header.
	if r.Method != http.MethodGet && r.Header.Get("Origin") != s.connections.origin {
		connectionJSON(w, 403, map[string]string{"error": "origin_not_allowed"})
		return connectionIdentity{}, false
	}
	i, ok := s.connectionBrowser(r)
	if !ok {
		connectionJSON(w, 401, map[string]string{"error": "browser_sign_in_required"})
	}
	return i, ok
}

func (s *Server) handleConnectRequest(w http.ResponseWriter, r *http.Request) {
	i, ok := s.requireConnectionBrowser(w, r)
	if !ok {
		return
	}
	req, err := s.connections.store.Inspect(r.Context(), r.URL.Query().Get("user_code"), s.connections.publicURL)
	if err != nil {
		connectionError(w, err)
		return
	}
	account := "Standalone administrator"
	if s.member != nil {
		account = fmt.Sprintf("Mesh member %d (%s)", i.id, i.role)
		if id, name, valid := s.browserAccount(r); valid && id == i.id && name != "" {
			account = name + " (" + i.role + ")"
		}
		if i.id < 0 {
			account = "Break-glass administrator"
		}
	}
	connectionJSON(w, 200, struct {
		connectauth.Request
		Account string `json:"account"`
		Role    string `json:"role"`
	}{req, account, i.role})
}

func (s *Server) handleConnectDecision(w http.ResponseWriter, r *http.Request) {
	i, ok := s.requireConnectionBrowser(w, r)
	if !ok {
		return
	}
	in, ok := connectionBody(w, r)
	if !ok {
		return
	}
	if in.Decision != "approve" && in.Decision != "deny" {
		connectionError(w, connectauth.ErrInvalid)
		return
	}
	if err := s.connections.store.Decide(r.Context(), in.UserCode, i.subject, s.connections.publicURL, in.Scope, in.Decision == "approve"); err != nil {
		connectionError(w, err)
		return
	}
	connectionJSON(w, 200, map[string]string{"decision": in.Decision})
}

func (s *Server) handleConnectList(w http.ResponseWriter, r *http.Request) {
	i, ok := s.requireConnectionBrowser(w, r)
	if !ok {
		return
	}
	grants, err := s.connections.store.List(r.Context(), i.subject, s.connections.publicURL)
	if err != nil {
		connectionError(w, err)
		return
	}
	connectionJSON(w, 200, grants)
}

func (s *Server) handleConnectDisconnect(w http.ResponseWriter, r *http.Request) {
	i, ok := s.requireConnectionBrowser(w, r)
	if !ok {
		return
	}
	in, ok := connectionBody(w, r)
	if !ok {
		return
	}
	if err := s.connections.store.Revoke(r.Context(), in.GrantID, i.subject, s.connections.publicURL); err != nil {
		connectionError(w, err)
		return
	}
	connectionJSON(w, 200, map[string]bool{"revoked": true})
}

func (s *Server) handleConnectPage(w http.ResponseWriter, r *http.Request) {
	if !s.connectionEnabled(w) {
		return
	}
	body, err := assetsFS.ReadFile("assets/connect.html")
	if err != nil {
		http.Error(w, "unavailable", 500)
		return
	}
	tmpl, err := template.New("connect").Parse(string(body))
	if err != nil {
		http.Error(w, "unavailable", 500)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = tmpl.Execute(w, struct{ Base, SignInURL string }{s.basePath + "/", s.browserSignInURL(r)})
}
