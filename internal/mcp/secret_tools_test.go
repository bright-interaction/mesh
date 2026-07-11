// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The plaintext values a leak test must never find in a tool result.
const (
	fakeAPIKey      = "dk_never_leak_this_key"
	fakeSecretValue = "SUPERSECRET-openai-key-value"
)

// fakeVault stands up the two Dockyard endpoints Mesh calls. The list response
// intentionally includes a value-shaped field, and issue returns only a token + the
// secret NAME, so the leak assertions below are meaningful.
func fakeVault(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/vault/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != fakeAPIKey {
			t.Errorf("list X-API-Key = %q", r.Header.Get("X-API-Key"))
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "v1", "name": "openai", "provider": "openai", "category": "llm",
				"auto_rotate": true, "rotation_status": "ok",
				"encrypted_value": fakeSecretValue}, // must never surface
		})
	})
	mux.HandleFunc("/api/secrets/issue", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != fakeAPIKey {
			t.Errorf("issue X-API-Key = %q", r.Header.Get("X-API-Key"))
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": "cap.tok.sig", "proxy_url": "http://wrong-host/proxy",
			"secret": "openai", "expires_at": "2026-07-11T00:05:00Z", "nonce_length": 16,
		})
	})
	return httptest.NewServer(mux)
}

// assertNoSecretLeak serializes a tool result and fails if it contains the Dockyard API
// key or the secret value: the core invariant (only names + a capability token ever
// cross into Mesh).
func assertNoSecretLeak(t *testing.T, label string, out map[string]any) {
	t.Helper()
	blob, _ := json.Marshal(out)
	if strings.Contains(string(blob), fakeAPIKey) {
		t.Fatalf("%s leaked the Dockyard API key: %s", label, blob)
	}
	if strings.Contains(string(blob), fakeSecretValue) {
		t.Fatalf("%s leaked a secret value: %s", label, blob)
	}
}

// TestSecretToolsUnconfigured: with no bridge configured, the tools are advertised but
// return a graceful not-configured note (never an error), like mesh_code_search on an
// empty index.
func TestSecretToolsUnconfigured(t *testing.T) {
	t.Setenv("MESH_SECRET_BRIDGE_URL", "")
	t.Setenv("MESH_SECRET_BRIDGE_KEY", "")
	s := newTestServer(t)

	for _, name := range []string{"mesh_secret_status", "mesh_secret_list"} {
		out := toolText(t, call(t, s, "tools/call", map[string]any{"name": name, "arguments": map[string]any{}}))
		if out["configured"] != false {
			t.Fatalf("%s should report configured=false, got %v", name, out)
		}
	}
	out := toolText(t, call(t, s, "tools/call", map[string]any{
		"name": "mesh_secret_use", "arguments": map[string]any{"destination": "api.x.com/y"},
	}))
	if out["configured"] != false {
		t.Fatalf("use should report configured=false, got %v", out)
	}
}

// TestSecretBrokerFlow: with a bridge configured, list returns names only and use
// returns a capability token + a proxy URL built from the CONFIGURED base (not the
// wrong-host proxy_url Dockyard echoed). Neither result carries the key or a value.
func TestSecretBrokerFlow(t *testing.T) {
	fake := fakeVault(t)
	defer fake.Close()
	t.Setenv("MESH_SECRET_BRIDGE_URL", fake.URL)
	t.Setenv("MESH_SECRET_BRIDGE_KEY", fakeAPIKey)
	s := newTestServer(t)

	st := toolText(t, call(t, s, "tools/call", map[string]any{"name": "mesh_secret_status", "arguments": map[string]any{}}))
	if st["configured"] != true {
		t.Fatalf("status configured=%v, want true", st["configured"])
	}
	if !strings.HasPrefix(st["agent_id"].(string), "mesh-") {
		t.Fatalf("agent_id = %v, want mesh-<host>", st["agent_id"])
	}

	ls := toolText(t, call(t, s, "tools/call", map[string]any{"name": "mesh_secret_list", "arguments": map[string]any{}}))
	if ls["count"].(float64) != 1 {
		t.Fatalf("list count = %v, want 1", ls["count"])
	}
	assertNoSecretLeak(t, "list", ls)

	us := toolText(t, call(t, s, "tools/call", map[string]any{
		"name": "mesh_secret_use", "arguments": map[string]any{"destination": "api.openai.com/v1/chat/completions", "method": "POST"},
	}))
	if us["token"] != "cap.tok.sig" {
		t.Fatalf("use token = %v", us["token"])
	}
	if us["secret_used"] != "openai" { // the NAME, not a value
		t.Fatalf("secret_used = %v", us["secret_used"])
	}
	proxy, _ := us["proxy_url"].(string)
	want := fake.URL + "/proxy/api.openai.com/v1/chat/completions"
	if proxy != want {
		t.Fatalf("proxy_url = %q, want %q (built from configured base, not the echoed host)", proxy, want)
	}
	assertNoSecretLeak(t, "use", us)
}

// TestSecretUseWriteGated: minting a capability token spends the team's credential, so a
// read-only hosted viewer must be forbidden, while a writer (and a solo binary with no
// policy) may broker.
func TestSecretUseWriteGated(t *testing.T) {
	fake := fakeVault(t)
	defer fake.Close()
	t.Setenv("MESH_SECRET_BRIDGE_URL", fake.URL)
	t.Setenv("MESH_SECRET_BRIDGE_KEY", fakeAPIKey)
	s := newTestServer(t)

	ro := WithWriteCapability(context.Background(), false)
	if _, rerr := s.toolSecretUse(ro, json.RawMessage(`{"destination":"api.openai.com/v1/chat/completions"}`)); rerr == nil {
		t.Fatal("a read-only viewer must be forbidden from brokering secrets")
	}

	rw := WithWriteCapability(context.Background(), true)
	out, rerr := s.toolSecretUse(rw, json.RawMessage(`{"destination":"api.openai.com/v1/chat/completions"}`))
	if rerr != nil {
		t.Fatalf("a writer must be allowed to broker: %v", rerr)
	}
	if m := toolText(t, out.(map[string]any)); m["token"] != "cap.tok.sig" {
		t.Fatalf("writer broker missing token: %v", m)
	}

	// Solo binary (no write policy set): unrestricted.
	out, rerr = s.toolSecretUse(context.Background(), json.RawMessage(`{"destination":"api.openai.com/v1/chat/completions"}`))
	if rerr != nil {
		t.Fatalf("solo (no policy) must broker: %v", rerr)
	}
	_ = out
}

// TestSecretUseRequiresDestination: a missing destination is a clean validation error.
func TestSecretUseRequiresDestination(t *testing.T) {
	fake := fakeVault(t)
	defer fake.Close()
	t.Setenv("MESH_SECRET_BRIDGE_URL", fake.URL)
	t.Setenv("MESH_SECRET_BRIDGE_KEY", fakeAPIKey)
	s := newTestServer(t)
	if _, rerr := s.toolSecretUse(context.Background(), json.RawMessage(`{}`)); rerr == nil {
		t.Fatal("missing destination must be a validation error")
	}
}
