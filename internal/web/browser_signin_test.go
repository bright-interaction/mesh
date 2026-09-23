// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestBrowserSignInConsentBoundary(t *testing.T) {
	s, _, _ := connectionServer(t)
	alive := true
	s.SetMemberAuth(func(string) (int64, string, bool) { return 0, "", false }, func(int64) map[string]bool { return map[string]bool{} }, nil,
		func(id int64) (string, int64, bool) { return "viewer", 123, alive && id == 7 })
	bridge := BrowserSignIn{LoginURL: connectionOrigin + "/auth/oidc/login", Resolve: func(r *http.Request) (int64, string, bool) {
		c, err := r.Cookie("fixture_team")
		return 7, "alice@example.test", err == nil && c.Value == "fixture-session"
	}, Clear: func(w http.ResponseWriter) { http.SetCookie(w, &http.Cookie{Name: "fixture_team", MaxAge: -1}) }}
	for _, bad := range []string{"https://evil.example/auth/oidc/login", "http://mesh.example.test/auth/oidc/login", connectionOrigin + "/auth/oidc/login?next=evil", connectionOrigin + "/auth/oidc/login#evil", connectionOrigin + "/api/login"} {
		bridge.LoginURL = bad
		if err := s.SetBrowserSignIn(bridge); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	bridge.LoginURL = connectionOrigin + "/auth/oidc/login"
	if err := s.SetBrowserSignIn(bridge); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	root := connectionCall(t, h, "GET", "/", nil, nil, "", "")
	if root.Code != 200 || !strings.Contains(root.Body.String(), "/auth/oidc/login?return_to=%2Fapp%2F") || strings.Contains(root.Body.String(), "__MESH_SIGN_IN__") {
		t.Fatal("viewer shell missing configured account sign-in")
	}
	d := connectionDevice(t, h, "full")
	page := connectionCall(t, h, "GET", "/connect?user_code="+d.UserCode, nil, nil, "", "")
	if page.Code != 200 || !strings.Contains(page.Body.String(), "Continue with your account") || !strings.Contains(page.Body.String(), "return_to=") {
		t.Fatal("missing account sign-in")
	}
	u, err := url.Parse(s.browserSignInURL(httptest.NewRequest("GET", "/connect?user_code="+d.UserCode, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("return_to") != "/app/connect?user_code="+d.UserCode {
		t.Fatal("lost pending code")
	}
	cookie := &http.Cookie{Name: "fixture_team", Value: "fixture-session"}
	w := connectionCall(t, h, "GET", "/api/connect/request?user_code="+d.UserCode, nil, cookie, "", "")
	var details map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &details)
	if w.Code != 200 || details["account"] != "alice@example.test (viewer)" {
		t.Fatalf("account not resolved: %d %s", w.Code, w.Body.String())
	}
	// Sign-in and request inspection must not grant access automatically.
	w = connectionCall(t, h, "GET", "/api/connect/connections", nil, cookie, "", "")
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("unexpected grants: %s", w.Body.String())
	}
	if w = connectionCall(t, h, "POST", "/api/connect/decision", map[string]string{"user_code": d.UserCode, "scope": "full", "decision": "approve"}, cookie, "", "https://evil.example"); w.Code != 403 {
		t.Fatal("foreign origin allowed")
	}
	// Fresh SSO must work even if an old viewer cookie is expired or corrupt.
	r := httptest.NewRequest("GET", connectionOrigin+"/app/api/connect/request?user_code="+d.UserCode, nil)
	r.AddCookie(cookie)
	r.AddCookie(&http.Cookie{Name: memberCookie, Value: "7.1.stale"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("stale viewer cookie shadowed SSO: %d", rec.Code)
	}
	alive = false
	if w = connectionCall(t, h, "GET", "/api/connect/request?user_code="+d.UserCode, nil, cookie, "", ""); w.Code != 401 {
		t.Fatal("removed member retained browser access")
	}
	alive = true
	for _, path := range []string{"/api/login", "/api/logout"} {
		if w = connectionCall(t, h, "POST", path, map[string]string{}, cookie, "", "https://evil.example"); w.Code != 403 {
			t.Fatalf("cross-origin browser auth mutation allowed: %s", path)
		}
	}
	if w = connectionCall(t, h, "POST", "/api/config", map[string]string{}, cookie, "", ""); w.Code != 403 {
		t.Fatal("SSO mutation without Origin allowed")
	}
	w = connectionCall(t, h, "POST", "/api/logout", nil, cookie, "", connectionOrigin)
	cleared := false
	for _, c := range w.Result().Cookies() {
		if c.Name == "fixture_team" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("viewer logout retained team cookie")
	}
}
