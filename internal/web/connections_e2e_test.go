// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"context"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Opt-in: starts only disposable loopback TLS, trusts its certificate only in
// the client subprocess, and exercises the actual shipped Go and IDE handlers.
func TestConnectionIDEHTTPSJourney(t *testing.T) {
	if os.Getenv("MESH_CONNECT_E2E") != "1" {
		t.Skip("set MESH_CONNECT_E2E=1 for actual Go/IDE HTTPS integration")
	}
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatal("the explicit IDE integration run requires Bun")
	}
	s, _ := cfgServer(t)
	s.auth = authConfig{token: "isolated-connection-e2e-fixture-only"}
	s.basePath = "/app"
	server := httptest.NewUnstartedServer(nil)
	defer server.Close()
	base := "https://" + server.Listener.Addr().String() + "/app"
	if err = s.EnableConnections(base, filepath.Join(t.TempDir(), "auth", "connections.db")); err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = s.Handler()
	server.StartTLS()
	cert := filepath.Join(t.TempDir(), "fixture-ca.pem")
	if err = os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bun, filepath.Join("..", "..", "ide", "integration", "connection-e2e.cjs"))
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "NODE_EXTRA_CA_CERTS=") && !strings.HasPrefix(value, "NODE_TLS_REJECT_UNAUTHORIZED=") && !strings.HasPrefix(value, "MESH_E2E_") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "NODE_EXTRA_CA_CERTS="+cert, "MESH_E2E_BASE="+base, "MESH_E2E_KEY="+s.auth.token)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated IDE HTTPS journey failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "PASS Mesh Go/IDE HTTPS journey") {
		t.Fatal("missing client completion receipt")
	}
	t.Log(strings.TrimSpace(string(output)))
}
