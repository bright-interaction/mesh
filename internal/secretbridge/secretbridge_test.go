// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package secretbridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeDockyard stands up the two endpoints Mesh calls, asserting the auth header and
// echoing back a plausible list + issue response. It deliberately never returns a
// secret VALUE (Dockyard does not either), so the leak assertions are meaningful.
func fakeDockyard(t *testing.T, wantKey string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/vault/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("list method = %s, want GET", r.Method)
		}
		if got := r.Header.Get("X-API-Key"); got != wantKey {
			t.Errorf("list X-API-Key = %q, want %q", got, wantKey)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("list must use X-API-Key, not Authorization; got %q", got)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "v1", "name": "openai", "provider": "openai", "category": "llm",
				"last_rotated_at": "2026-07-01T00:00:00Z", "auto_rotate": true, "rotation_status": "ok",
				// A value-shaped field the client must NOT surface even if present:
				"encrypted_value": "SHOULD-NEVER-APPEAR"},
		})
	})
	mux.HandleFunc("/api/secrets/issue", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("issue method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("X-API-Key"); got != wantKey {
			t.Errorf("issue X-API-Key = %q, want %q", got, wantKey)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		if req["agent_id"] != "mesh-test" {
			t.Errorf("issue agent_id = %v, want mesh-test", req["agent_id"])
		}
		if req["destination"] != "api.openai.com/v1/chat/completions" {
			t.Errorf("issue destination = %v", req["destination"])
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{ //gitleaks:allow (fixture token, not a real credential)
			"token": "cap.faketoken.fakesig", "proxy_url": "http://internal-wrong-host/proxy",
			"secret": "openai", "expires_at": "2026-07-11T00:05:00Z", "nonce_length": 16,
		})
	})
	return httptest.NewServer(mux)
}

func TestListSecretsProjectsMetadataOnly(t *testing.T) {
	srv := fakeDockyard(t, "dk_testkey")
	defer srv.Close()
	c := New(srv.URL, "dk_testkey", "mesh-test")

	metas, err := c.ListSecrets(context.Background())
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if len(metas) != 1 || metas[0].Name != "openai" || metas[0].Provider != "openai" {
		t.Fatalf("metas = %+v", metas)
	}
	if !metas[0].AutoRotate || metas[0].RotationStatus != "ok" {
		t.Fatalf("rotation metadata dropped: %+v", metas[0])
	}
	// The projected struct must not carry any value-shaped field.
	blob, _ := json.Marshal(metas)
	if strings.Contains(string(blob), "SHOULD-NEVER-APPEAR") {
		t.Fatalf("list leaked a value-shaped field: %s", blob)
	}
}

func TestIssueReturnsTokenNotSecret(t *testing.T) {
	srv := fakeDockyard(t, "dk_testkey")
	defer srv.Close()
	c := New(srv.URL, "dk_testkey", "mesh-test")

	res, err := c.Issue(context.Background(), IssueRequest{
		Destination: "api.openai.com/v1/chat/completions", Method: "POST",
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if res.Token != "cap.faketoken.fakesig" {
		t.Fatalf("token = %q", res.Token)
	}
	if res.Secret != "openai" { // the NAME, not a value
		t.Fatalf("secret name = %q", res.Secret)
	}
	// The whole response, serialized, must never contain the API key.
	blob, _ := json.Marshal(res)
	if strings.Contains(string(blob), "dk_testkey") {
		t.Fatalf("issue response leaked the API key: %s", blob)
	}
}

// TestProxyURLBuiltFromBaseURL: the client must build the proxy URL from the CONFIGURED
// base URL, not from the (wrong-host) proxy_url Dockyard echoes back.
func TestProxyURLBuiltFromBaseURL(t *testing.T) {
	c := New("https://dockyard.example.com/", "dk_x", "mesh-test")
	got := c.ProxyURL(SplitHostPath("api.openai.com/v1/chat/completions"))
	want := "https://dockyard.example.com/proxy/api.openai.com/v1/chat/completions"
	if got != want {
		t.Fatalf("ProxyURL = %q, want %q", got, want)
	}
}

func TestSplitHostPath(t *testing.T) {
	cases := []struct{ in, host, path string }{
		{"api.openai.com/v1/chat", "api.openai.com", "/v1/chat"},
		{"api.stripe.com", "api.stripe.com", ""},
		{"https://API.Cloudflare.com/client/v4/zones", "api.cloudflare.com", "/client/v4/zones"},
		{"/leading/slash.com/x", "leading", "/slash.com/x"},
		// A "://" inside a query value must NOT be mistaken for a leading scheme.
		{"api.example.com/cb?next=https://x", "api.example.com", "/cb?next=https://x"},
	}
	for _, tc := range cases {
		h, p := SplitHostPath(tc.in)
		if h != tc.host || p != tc.path {
			t.Errorf("SplitHostPath(%q) = (%q,%q), want (%q,%q)", tc.in, h, p, tc.host, tc.path)
		}
	}
}

// TestErrorSurfacesDockyardCode: a non-2xx surfaces Dockyard's error code (so the
// agent can adjust) without leaking the key, and no partial struct is returned.
func TestErrorSurfacesDockyardCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "ambiguous_destination"})
	}))
	defer srv.Close()
	c := New(srv.URL, "dk_secret_key", "mesh-test")
	_, err := c.Issue(context.Background(), IssueRequest{Destination: "api.x.com"})
	if err == nil {
		t.Fatal("want error on 409")
	}
	if !strings.Contains(err.Error(), "ambiguous_destination") {
		t.Fatalf("error should carry the dockyard code: %v", err)
	}
	if strings.Contains(err.Error(), "dk_secret_key") {
		t.Fatalf("error leaked the API key: %v", err)
	}
}

// TestClientRefusesRedirects: a compromised / MITM'd / open-redirecting Dockyard
// returning a 3xx must NOT cause Mesh to re-dial the redirect target (Go copies the
// custom X-API-Key header across redirects, so following one would egress the key +
// give an SSRF primitive). The client must surface the 3xx as an error and never touch
// the redirect location.
func TestClientRefusesRedirects(t *testing.T) {
	var hitTarget bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitTarget = true
		if k := r.Header.Get("X-API-Key"); k != "" {
			t.Errorf("API key leaked to redirect target: %q", k)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/", http.StatusFound)
	}))
	defer redir.Close()

	c := New(redir.URL, "dk_key_must_not_leak", "mesh-test")
	if _, err := c.ListSecrets(context.Background()); err == nil {
		t.Fatal("a 3xx from the bridge must surface as an error, not be followed")
	} else if strings.Contains(err.Error(), "dk_key_must_not_leak") {
		t.Fatalf("error leaked the API key: %v", err)
	}
	if hitTarget {
		t.Fatal("client followed the redirect to another host (key-egress / SSRF path)")
	}
}

// TestMissingConfigNoRequest: with no base URL or no key, the client fails fast
// without dialing anything.
func TestMissingConfigNoRequest(t *testing.T) {
	if _, err := New("", "k", "a").ListSecrets(context.Background()); err == nil {
		t.Fatal("want error with no base URL")
	}
	if _, err := New("https://x.example.com", "", "a").ListSecrets(context.Background()); err == nil {
		t.Fatal("want error with no API key")
	}
}
