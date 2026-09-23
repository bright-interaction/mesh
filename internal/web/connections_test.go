// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/connectauth"
)

const connectionOrigin = "https://mesh.example.test"

func connectionServer(t *testing.T) (*Server, http.Handler, *http.Cookie) {
	t.Helper()
	s, _ := cfgServer(t)
	s.auth = authConfig{token: "test-only-shared-viewer-secret"}
	s.basePath = "/app"
	if err := s.EnableConnections(connectionOrigin+"/app", filepath.Join(t.TempDir(), "auth", "connections.db")); err != nil {
		t.Fatal(err)
	}
	return s, s.Handler(), &http.Cookie{Name: sessionCookie, Value: s.auth.sessionValue()}
}

func connectionCall(t *testing.T, h http.Handler, method, path string, body any, cookie *http.Cookie, token, origin string) *httptest.ResponseRecorder {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, connectionOrigin+"/app"+path, strings.NewReader(string(data)))
	r.RemoteAddr = "192.0.2.17:3000"
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func connectionDevice(t *testing.T, h http.Handler, scope string) connectauth.Device {
	t.Helper()
	w := connectionCall(t, h, "POST", "/api/connect/device", map[string]string{"client_id": "mesh-ide", "scope": scope}, nil, "", "")
	if w.Code != 200 {
		t.Fatalf("device: %d %s", w.Code, w.Body.String())
	}
	var d connectauth.Device
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	if raw["verification_uri"] != connectionOrigin+"/app/connect" || strings.Contains(raw["verification_uri_complete"].(string), d.DeviceCode) {
		t.Fatal("unsafe approval URL")
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("credential response may be cached")
	}
	return d
}

func connectionApprove(t *testing.T, h http.Handler, cookie *http.Cookie, scope string) connectauth.Tokens {
	t.Helper()
	d := connectionDevice(t, h, scope)
	w := connectionCall(t, h, "POST", "/api/connect/decision", map[string]string{"user_code": d.UserCode, "scope": scope, "decision": "approve"}, cookie, "", connectionOrigin)
	if w.Code != 200 {
		t.Fatalf("decision: %d %s", w.Code, w.Body.String())
	}
	w = connectionCall(t, h, "POST", "/api/connect/token", map[string]string{"client_id": "mesh-ide", "device_code": d.DeviceCode, "grant_type": "urn:ietf:params:oauth:grant-type:device_code"}, nil, "", "")
	if w.Code != 200 {
		t.Fatalf("exchange: %d %s", w.Code, w.Body.String())
	}
	var tokens connectauth.Tokens
	if err := json.Unmarshal(w.Body.Bytes(), &tokens); err != nil {
		t.Fatal(err)
	}
	return tokens
}

func TestConnectionHTTPApprovalRefreshAndRevocation(t *testing.T) {
	_, h, cookie := connectionServer(t)
	tokens := connectionApprove(t, h, cookie, connectauth.ScopeFull)
	w := connectionCall(t, h, "GET", "/api/status", nil, nil, tokens.AccessToken, "")
	if w.Code != 200 {
		t.Fatalf("delegated read: %d %s", w.Code, w.Body.String())
	}
	w = connectionCall(t, h, "POST", "/api/connect/token", map[string]string{"client_id": "mesh-ide", "refresh_token": tokens.RefreshToken, "grant_type": "refresh_token"}, nil, "", "")
	if w.Code != 200 {
		t.Fatalf("refresh: %d %s", w.Code, w.Body.String())
	}
	var next connectauth.Tokens
	_ = json.Unmarshal(w.Body.Bytes(), &next)
	if next.GrantID != tokens.GrantID || next.AccessToken == tokens.AccessToken || next.Scope != connectauth.ScopeFull {
		t.Fatal("rotation changed grant or scope")
	}
	w = connectionCall(t, h, "GET", "/api/connect/connections", nil, cookie, "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), tokens.GrantID) || strings.Contains(w.Body.String(), next.AccessToken) {
		t.Fatal("unsafe connection listing")
	}
	w = connectionCall(t, h, "POST", "/api/connect/disconnect", map[string]string{"grant_id": tokens.GrantID}, cookie, "", connectionOrigin)
	if w.Code != 200 {
		t.Fatal("revoke failed")
	}
	if w = connectionCall(t, h, "GET", "/api/status", nil, nil, next.AccessToken, ""); w.Code != 401 {
		t.Fatal("revoked access accepted")
	}
	if w = connectionCall(t, h, "POST", "/api/connect/token", map[string]string{"client_id": "mesh-ide", "refresh_token": next.RefreshToken, "grant_type": "refresh_token"}, nil, "", ""); w.Code != 400 {
		t.Fatal("revoked refresh accepted")
	}
}

func TestConnectionConsentRequiresBrowserSessionAndExactOrigin(t *testing.T) {
	s, h, cookie := connectionServer(t)
	d := connectionDevice(t, h, connectauth.ScopeFull)
	body := map[string]string{"user_code": d.UserCode, "scope": "full", "decision": "approve"}
	for _, origin := range []string{"", "null", "https://evil.example", connectionOrigin + ".evil.example"} {
		if w := connectionCall(t, h, "POST", "/api/connect/decision", body, cookie, "", origin); w.Code != 403 {
			t.Fatalf("origin %q accepted: %d", origin, w.Code)
		}
	}
	if w := connectionCall(t, h, "POST", "/api/connect/decision", body, nil, s.auth.token, connectionOrigin); w.Code != 401 {
		t.Fatalf("raw bearer approved without browser session: %d", w.Code)
	}
	tokens := connectionApprove(t, h, cookie, connectauth.ScopeFull)
	if w := connectionCall(t, h, "POST", "/api/connect/decision", body, nil, tokens.AccessToken, connectionOrigin); w.Code != 401 {
		t.Fatalf("delegated bearer minted another grant: %d", w.Code)
	}
	if w := connectionCall(t, h, "POST", "/api/connect/decision", body, cookie, "", connectionOrigin); w.Code != 200 {
		t.Fatalf("valid browser rejected: %d %s", w.Code, w.Body.String())
	}
}

func TestConnectionReadScopeBlocksWritesAndRotationInvalidatesParent(t *testing.T) {
	s, h, cookie := connectionServer(t)
	tokens := connectionApprove(t, h, cookie, connectauth.ScopeRead)
	if w := connectionCall(t, h, "POST", "/api/reindex", map[string]string{}, nil, tokens.AccessToken, ""); w.Code != 403 {
		t.Fatalf("read connection can mutate: %d", w.Code)
	}
	s.auth.token = "rotated-viewer-secret"
	if w := connectionCall(t, h, "GET", "/api/status", nil, nil, tokens.AccessToken, ""); w.Code != 401 {
		t.Fatalf("parent rotation did not invalidate connection: %d", w.Code)
	}
	if w := connectionCall(t, h, "POST", "/api/connect/token", map[string]string{"client_id": "mesh-ide", "refresh_token": tokens.RefreshToken, "grant_type": "refresh_token"}, nil, "", ""); w.Code != 400 {
		t.Fatal("rotated parent renewed")
	}
}

func TestConnectionMemberRoleScopesAndRevocationRemainLive(t *testing.T) {
	s, _, _ := connectionServer(t)
	alive, generation, role := true, int64(100), "member"
	s.SetMemberAuth(func(string) (int64, string, bool) { return 0, "", false }, func(int64) map[string]bool { return map[string]bool{"permitted": true} }, func(int64) func(string) bool { return func(string) bool { return false } }, func(id int64) (string, int64, bool) { return role, generation, alive && id == 7 })
	h := s.Handler()
	cookie := &http.Cookie{Name: memberCookie, Value: s.member.sign(7, generation)}
	tokens := connectionApprove(t, h, cookie, connectauth.ScopeFull)
	if w := connectionCall(t, h, "POST", "/api/reindex", map[string]string{}, nil, tokens.AccessToken, ""); w.Code != 403 {
		t.Fatalf("full scope promoted member to admin: %d", w.Code)
	}
	w := connectionCall(t, h, "GET", "/graph.json", nil, nil, tokens.AccessToken, "")
	if w.Code != 200 {
		t.Fatalf("scoped graph: %d %s", w.Code, w.Body.String())
	}
	var graph struct {
		Nodes []any `json:"nodes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &graph); err != nil || len(graph.Nodes) != 0 {
		t.Fatal("denied folder data escaped")
	}
	role = "viewer"
	if w = connectionCall(t, h, "GET", "/api/status", nil, nil, tokens.AccessToken, ""); w.Code != 200 {
		t.Fatal("valid role downgrade invalidated read")
	}
	alive = false
	if w = connectionCall(t, h, "GET", "/api/status", nil, nil, tokens.AccessToken, ""); w.Code != 401 {
		t.Fatal("removed member retained access")
	}
	alive = true
	generation++
	if w = connectionCall(t, h, "POST", "/api/connect/token", map[string]string{"client_id": "mesh-ide", "refresh_token": tokens.RefreshToken, "grant_type": "refresh_token"}, nil, "", ""); w.Code != 400 {
		t.Fatal("reused identity renewed old grant")
	}
}

func TestConnectionConfiguredAudienceDoesNotTrustHostOrForwarded(t *testing.T) {
	s, h, cookie := connectionServer(t)
	tokens := connectionApprove(t, h, cookie, connectauth.ScopeFull)
	r := httptest.NewRequest("GET", "https://evil.example/app/api/status", nil)
	r.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	r.Header.Set("X-Forwarded-Host", "mesh.example.test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("forwarded host bypassed audience")
	}
	// Existing internal health/legacy-key requests do not need public Host spoofing.
	r = httptest.NewRequest("GET", "http://127.0.0.1:7474/app/api/status", nil)
	r.Header.Set("Authorization", "Bearer "+s.auth.token)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("legacy health broken: %d", w.Code)
	}
}

func TestConnectionPageNoAutoApprovalAndExplicitConfig(t *testing.T) {
	s, h, cookie := connectionServer(t)
	d := connectionDevice(t, h, connectauth.ScopeFull)
	w := connectionCall(t, h, "GET", "/connect?user_code="+d.UserCode, nil, cookie, "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `<base href="/app/">`) || !strings.Contains(w.Body.String(), `id="confirm-code"`) || strings.Contains(w.Body.String(), d.DeviceCode) {
		t.Fatal("invalid consent page")
	}
	if w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("consent headers missing")
	}
	if _, err := s.connections.store.Inspect(t.Context(), d.UserCode, connectionOrigin+"/app"); err != nil {
		t.Fatal("GET consumed approval")
	}
	for _, raw := range []string{"http://mesh.example.test/app", "https://user:pass@mesh.example.test/app", "https://mesh.example.test/wrong", "https://mesh.example.test/app?x=1"} {
		plain := &Server{auth: authConfig{token: "secret"}, basePath: "/app"}
		if err := plain.EnableConnections(raw, filepath.Join(t.TempDir(), "connections.db")); err == nil {
			plain.connections.store.Close()
			t.Fatalf("bad public URL accepted: %s", raw)
		}
	}
	plain := &Server{basePath: "/app"}
	if err := plain.EnableConnections(connectionOrigin+"/app", filepath.Join(t.TempDir(), "connections.db")); err == nil {
		plain.connections.store.Close()
		t.Fatal("unauthenticated approval enabled")
	}
}

func TestConnectionBadInputsCancelAndRateLimit(t *testing.T) {
	_, h, _ := connectionServer(t)
	d := connectionDevice(t, h, "read")
	w := connectionCall(t, h, "POST", "/api/connect/cancel", map[string]string{"client_id": "mesh-ide", "device_code": d.DeviceCode}, nil, "", "")
	if w.Code != 200 {
		t.Fatal("cancel failed")
	}
	w = connectionCall(t, h, "POST", "/api/connect/token", map[string]string{"client_id": "mesh-ide", "device_code": d.DeviceCode, "grant_type": "urn:ietf:params:oauth:grant-type:device_code"}, nil, "", "")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_grant") {
		t.Fatal("cancelled request accepted")
	}
	for _, body := range []string{`{"client_id":"mesh-ide","scope":"read","subject":"admin"}`, `{"client_id":"mesh-ide","scope":"read"} {}`, strings.Repeat("x", 9000)} {
		r := httptest.NewRequest("POST", connectionOrigin+"/app/api/connect/token", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("malformed request accepted: %d", w.Code)
		}
	}
	limited := false
	for i := 0; i < 8; i++ {
		w = connectionCall(t, h, "POST", "/api/connect/device", map[string]string{"client_id": "mesh-ide", "scope": "read"}, nil, "", "")
		if w.Code == 429 {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("device creation is unbounded")
	}
}
