// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewAuthConfigFailsClosed(t *testing.T) {
	// loopback needs no token.
	if _, err := newAuthConfig("127.0.0.1:7474", ""); err != nil {
		t.Errorf("loopback should not require a token: %v", err)
	}
	if _, err := newAuthConfig("localhost:7474", ""); err != nil {
		t.Errorf("localhost should not require a token: %v", err)
	}
	// non-loopback without a token is refused.
	if _, err := newAuthConfig("0.0.0.0:7474", ""); err == nil {
		t.Error("non-loopback without a token must be refused (fail closed)")
	}
	if _, err := newAuthConfig(":7474", ""); err == nil {
		t.Error("all-interfaces bind without a token must be refused")
	}
	// non-loopback WITH a token is allowed.
	if _, err := newAuthConfig("0.0.0.0:7474", "secret"); err != nil {
		t.Errorf("non-loopback with a token should be allowed: %v", err)
	}
}

func TestGuardTokenGate(t *testing.T) {
	a := authConfig{token: "s3cret", loopback: false}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /assets/x.js", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /graph.json", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := a.guard(mux)

	// graph payload is vault data: it must be gated too (not just /api/).
	grec := httptest.NewRecorder()
	h.ServeHTTP(grec, httptest.NewRequest("GET", "/graph.json", nil))
	if grec.Code != http.StatusUnauthorized {
		t.Errorf("graph.json without token = %d, want 401", grec.Code)
	}

	// /api without token -> 401
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("api without token = %d, want 401", rec.Code)
	}
	// /api with token -> 200
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("api with token = %d, want 200", rec.Code)
	}
	// assets stay open (the SPA must load to prompt for the token)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/assets/x.js", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("assets should be open = %d, want 200", rec.Code)
	}
	// wrong token -> 401
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/status", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("api with wrong token = %d, want 401", rec.Code)
	}
}

func TestCookieLoginFlow(t *testing.T) {
	s, _ := cfgServer(t)
	s.auth = authConfig{token: "s3cret", loopback: false}
	h := s.Handler()

	// login is reachable unauthenticated; a wrong key is refused with no cookie.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, jsonReq("POST", "/api/login", `{"key":"nope"}`))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login wrong key = %d, want 401", rec.Code)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Error("a rejected login must not set a cookie")
	}

	// the correct key sets a hardened HttpOnly session cookie.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, jsonReq("POST", "/api/login", `{"key":"s3cret"}`))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("login correct key = %d, want 204", rec.Code)
	}
	var sess *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			sess = c
		}
	}
	if sess == nil {
		t.Fatal("login did not set the session cookie")
	}
	if !sess.HttpOnly || !sess.Secure || sess.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie not hardened: HttpOnly=%v Secure=%v SameSite=%v", sess.HttpOnly, sess.Secure, sess.SameSite)
	}
	if sess.Value == "s3cret" {
		t.Error("cookie must not be the raw token (use the HMAC-derived value)")
	}
	if sess.Value != s.auth.sessionValue() {
		t.Error("cookie value is not the derived session value")
	}

	// the cookie authenticates a gated request (no Authorization header needed).
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.AddCookie(sess)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status with session cookie = %d, want 200", rec.Code)
	}

	// a tampered cookie is rejected.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/status", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sess.Value + "x"})
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status with tampered cookie = %d, want 401", rec.Code)
	}
}

func jsonReq(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}
