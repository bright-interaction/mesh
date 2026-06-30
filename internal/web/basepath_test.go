// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBasePathServing(t *testing.T) {
	s, _ := cfgServer(t)
	s.basePath = "/app"
	h := s.Handler()

	// API + assets are served under the base path.
	if code, _ := doJSON(t, h, "GET", "/app/api/status", ""); code != 200 {
		t.Errorf("/app/api/status = %d, want 200", code)
	}
	// Root paths are NOT served when a base path is set.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	if rec.Code == 200 {
		t.Errorf("/api/status should not resolve under a base path, got %d", rec.Code)
	}
	// The shell at /app/ carries the rewritten <base href>.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/app/", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `<base href="/app/">`) {
		t.Errorf("/app/ shell = %d, base href present=%v", rec.Code, strings.Contains(rec.Body.String(), `<base href="/app/">`))
	}
	// /app redirects to /app/ (subtree).
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/app", nil))
	if rec.Code < 300 || rec.Code >= 400 {
		t.Errorf("/app = %d, want a redirect to /app/", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasSuffix(loc, "/app/") {
		t.Errorf("/app redirect Location = %q, want .../app/", loc)
	}
}

func TestRootBaseHrefDefault(t *testing.T) {
	s, _ := cfgServer(t)
	h := s.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if !strings.Contains(rec.Body.String(), `<base href="/">`) {
		t.Errorf("root shell should have <base href=\"/\">")
	}
}
