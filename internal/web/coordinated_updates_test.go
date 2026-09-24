// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/updatecheck"
)

func TestCoordinatedUpdateStatusReleaseAndAuth(t *testing.T) {
	t.Setenv("MESH_RELEASE_VERSION", "v0.41.6")
	dir := t.TempDir()
	for _, fixture := range []struct{ name, scope string }{{"visible", "dev"}, {"private", "private"}} {
		body := "---\nid: " + fixture.name + "\ntype: note\nscope: " + fixture.scope + "\n---\n# " + fixture.name + "\n"
		if err := os.WriteFile(filepath.Join(dir, fixture.name+".md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	seedIndex(t, dir)
	s, err := NewServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.auth = authConfig{token: "update-test-only", loopback: false}
	s.SetScopeResolver(func(*http.Request) map[string]bool { return map[string]bool{"dev": true} })
	h := s.Handler()
	for _, mode := range []string{"disabled", "absent", "failed", "available", "current"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			var want updatecheck.Notice
			s.updateCheck = func(ctx context.Context, current string) (updatecheck.Notice, error) {
				calls++
				if current != "v0.41.6" {
					t.Fatalf("checker current = %q", current)
				}
				if mode == "disabled" {
					return updatecheck.Default.Check(ctx, current)
				}
				if mode == "failed" {
					return updatecheck.Notice{}, errors.New("fixture update discovery unavailable")
				}
				return want, nil
			}
			switch mode {
			case "disabled":
				t.Setenv("MESH_NO_UPDATE_CHECK", "1")
			case "absent":
				s.updateCheck = nil
			case "available":
				want = updatecheck.Notice{Available: true, Current: "v0.41.6", Latest: "v0.41.7", Command: "unchanged legacy command", PrebuiltCommand: "mesh upgrade", URL: "https://github.com/bright-interaction/mesh/tree/v0.41.7"}
			case "current":
				want = updatecheck.Notice{Current: "v0.41.6", Latest: "v0.41.6"}
			}
			for _, token := range []string{"", "wrong-test-token"} {
				req := httptest.NewRequest("GET", "/api/status", nil)
				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), "v0.41.6") || strings.Contains(rec.Body.String(), `"release"`) {
					t.Fatalf("unauthorized status disclosed release: %d %s", rec.Code, rec.Body.String())
				}
			}
			if calls != 0 {
				t.Fatal("unauthorized request invoked update discovery")
			}
			req := httptest.NewRequest("GET", "/api/status", nil)
			req.Header.Set("Authorization", "Bearer update-test-only")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("authorized status = %d: %s", rec.Code, rec.Body.String())
			}
			var got struct {
				Release string             `json:"release"`
				Update  updatecheck.Notice `json:"update"`
				Counts  map[string]int     `json:"counts"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Release != "v0.41.6" || got.Update != want || got.Counts["notes"] != 1 {
				t.Fatalf("release, legacy notice or scope regression: %+v; expected notice %+v and one visible note", got, want)
			}
		})
	}
}
