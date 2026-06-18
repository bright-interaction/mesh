package web

import (
	"net/http"
	"net/http/httptest"
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
	h := a.guard(mux)

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
