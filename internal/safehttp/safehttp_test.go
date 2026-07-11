// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package safehttp

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", // cloud metadata
		"100.64.0.1",      // CGNAT / Tailscale
		"100.100.100.100", // Tailscale MagicDNS
		"192.0.0.192",     // Oracle metadata
		"0.0.0.0",
		"::1",
		"fe80::1",
		"fc00::1",
		"::ffff:127.0.0.1", // IPv4-mapped loopback
	}
	for _, s := range blocked {
		if !BlockedIP(net.ParseIP(s)) {
			t.Errorf("BlockedIP(%s) = false, want true (SSRF target)", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "93.184.216.34"} {
		if BlockedIP(net.ParseIP(s)) {
			t.Errorf("BlockedIP(%s) = true, want false (public)", s)
		}
	}
	if !BlockedIP(nil) {
		t.Error("BlockedIP(nil) should be true")
	}
}

// The default LLM client refuses a loopback endpoint; with the operator opt-in it
// connects. This is the exact SSRF fix: a config-set embedding/rerank endpoint cannot
// probe the host unless the operator explicitly allows a sovereign localhost endpoint.
func TestLLMClientGuardAndOptIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, err := LLMClient(5 * time.Second).Get(srv.URL); err == nil {
		t.Fatal("default LLMClient reached a loopback endpoint; SSRF guard did not fire")
	}
	t.Setenv("MESH_ALLOW_PRIVATE_LLM_ENDPOINT", "1")
	resp, err := LLMClient(5 * time.Second).Get(srv.URL)
	if err != nil {
		t.Fatalf("opted-in LLMClient could not reach loopback: %v", err)
	}
	resp.Body.Close()
}
