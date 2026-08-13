// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package safehttp

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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

// An endpoint the OPERATOR supplied (a CLI flag, a process env var) is dialed even on
// loopback with no second opt-in: no HTTP surface can write a flag or the environment,
// so it already carries the authority MESH_ALLOW_PRIVATE_LLM_ENDPOINT expresses. This is
// the constructor split that made the documented local BYOAI setup work at all.
func TestOperatorLLMClientReachesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if AllowPrivateLLMEndpoint() {
		t.Fatal("precondition: MESH_ALLOW_PRIVATE_LLM_ENDPOINT must be unset for this test")
	}
	resp, err := OperatorLLMClient(5 * time.Second).Get(srv.URL)
	if err != nil {
		t.Fatalf("OperatorLLMClient could not reach a local endpoint: %v", err)
	}
	resp.Body.Close()
}

// Every refusal a stranger can trigger has to name its remedy. The BYOAI refusal named
// none: the opt-in appeared in no README, no doc, no .env.example and no error string,
// so `mesh embed` against a local Ollama dead-ended on "refusing to connect".
func TestLLMClientRefusalNamesTheOptIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	_, err := LLMClient(5 * time.Second).Get(srv.URL)
	if err == nil {
		t.Fatal("LLMClient reached a loopback endpoint; SSRF guard did not fire")
	}
	if !strings.Contains(err.Error(), "MESH_ALLOW_PRIVATE_LLM_ENDPOINT") {
		t.Errorf("the SSRF refusal must name the operator opt-in, got: %v", err)
	}
}

// The plain connector guard keeps its bare message: MESH_ALLOW_PRIVATE_LLM_ENDPOINT does
// nothing for an ingest connector, so naming it there would send the user somewhere that
// cannot help them.
func TestPlainClientRefusalDoesNotNameTheLLMOptIn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	_, err := Client(5 * time.Second).Get(srv.URL)
	if err == nil {
		t.Fatal("Client reached a loopback endpoint; SSRF guard did not fire")
	}
	if strings.Contains(err.Error(), "MESH_ALLOW_PRIVATE_LLM_ENDPOINT") {
		t.Errorf("the connector guard must not advertise the BYOAI opt-in, got: %v", err)
	}
}
