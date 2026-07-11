// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMemberRoleGateAndRevocation proves two things the per-member web mode must do:
//  1. state-changing routes (config/reindex/review-queue) require an admin role, so a
//     viewer/member is refused with 403 while an admin succeeds;
//  2. a removed member's still-valid cookie/token stops working immediately, because
//     every request re-checks that the client still exists (roleFor ok=false).
func TestMemberRoleGateAndRevocation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "n.md"), []byte("---\nid: n\ntype: note\ntitle: N\n---\n# n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	// exists is flipped to false to simulate the admin being removed mid-session.
	adminExists := true
	srv.SetMemberAuth(
		func(tok string) (int64, string, bool) {
			switch tok {
			case "admintok":
				return 1, "admin", true
			case "viewertok":
				return 2, "viewer", true
			}
			return 0, "", false
		},
		func(id int64) map[string]bool { return nil }, // unrestricted reads
		func(id int64) (string, bool) {
			switch id {
			case 1:
				if !adminExists {
					return "", false // revoked
				}
				return "admin", true
			case 2:
				return "viewer", true
			}
			return "", false
		},
	)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	put := func(tok string) int {
		req, _ := http.NewRequest("PUT", ts.URL+"/api/config", strings.NewReader(`{"updates":{}}`))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if st := put("viewertok"); st != http.StatusForbidden {
		t.Fatalf("viewer PUT /api/config: want 403, got %d", st)
	}
	if st := put("admintok"); st != http.StatusOK {
		t.Fatalf("admin PUT /api/config: want 200, got %d", st)
	}
	// Remove the admin; the same token must now be refused everywhere (revocation).
	adminExists = false
	if st := put("admintok"); st == http.StatusOK {
		t.Fatal("revoked admin token still succeeded on PUT /api/config")
	}
	// A revoked member is also unauthenticated for reads (the guard denies them).
	req, _ := http.NewRequest("GET", ts.URL+"/graph.json", nil)
	req.Header.Set("Authorization", "Bearer admintok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked admin GET /graph.json: want 401, got %d", resp.StatusCode)
	}
}
