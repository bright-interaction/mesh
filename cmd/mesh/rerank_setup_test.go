// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/hooks"
	"github.com/bright-interaction/mesh/internal/rerank"
)

func isolateRerankConfig(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
}

func fakeSubscriptionCLI(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	marker := filepath.Join(dir, "CALLED")
	body := "#!/bin/sh\nprintf called > " + marker + "\nexit 99\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return marker
}

func TestRerankSetupStatusDisableCodexWithoutModelCall(t *testing.T) {
	isolateRerankConfig(t)
	proj := t.TempDir()
	vault := t.TempDir()
	if _, _, err := hooks.RegisterMCP("codex", proj, "/vault", "/bin/mesh"); err != nil {
		t.Fatal(err)
	}
	marker := fakeSubscriptionCLI(t, "codex")

	out, err := runCLI(t, rerankSetupCmd(), vault, "--client", "codex", "--dir", proj)
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	for _, want := range []string{"gpt-5.6-luna", "effort: low", "policy: auto", "no model call was made", "compact cards"} {
		if !strings.Contains(out, want) {
			t.Errorf("setup receipt missing %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("setup executed the provider CLI; it must only perform a zero-quota path lookup")
	}

	out, err = runCLI(t, rerankStatusCmd(), vault)
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if !strings.Contains(out, "subscription rerank: on") || !strings.Contains(out, "authentication: not checked") {
		t.Fatalf("status is not explicit about readiness:\n%s", out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("status executed the provider CLI")
	}

	out, err = runCLI(t, rerankDisableCmd(), vault)
	if err != nil {
		t.Fatalf("disable: %v\n%s", err, out)
	}
	out, err = runCLI(t, rerankStatusCmd(), vault)
	if err != nil || !strings.Contains(out, "subscription rerank: off") {
		t.Fatalf("status after disable: %v\n%s", err, out)
	}
}

func TestRerankSetupInfersClaudeAndPinsHaiku(t *testing.T) {
	isolateRerankConfig(t)
	proj := t.TempDir()
	vault := t.TempDir()
	if _, _, err := hooks.RegisterMCP("claude-code", proj, "/vault", "/bin/mesh"); err != nil {
		t.Fatal(err)
	}
	mcpPath := filepath.Join(proj, ".mcp.json")
	before, _ := os.ReadFile(mcpPath)
	marker := fakeSubscriptionCLI(t, "claude")
	out, err := runCLI(t, rerankSetupCmd(), vault, "--client", "claude-code", "--dir", proj)
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if !strings.Contains(out, "claude-haiku-4-5-20251001") || !strings.Contains(out, "Claude CLI") {
		t.Fatalf("Claude setup did not pin Haiku:\n%s", out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("Claude setup executed the provider CLI")
	}
	after, _ := os.ReadFile(mcpPath)
	if string(after) != string(before) {
		t.Fatalf("setup wrote the private opt-in into Claude's project-shared .mcp.json:\n%s", after)
	}
}

func TestRerankSetupRequiresExplicitAgentForNeutralClient(t *testing.T) {
	if _, err := runCLI(t, rerankSetupCmd(), "--client", "cursor"); err == nil || !strings.Contains(err.Error(), "pass --agent") {
		t.Fatalf("neutral client should need an explicit provider, got %v", err)
	}
}

func TestInstallCanOptIntoSubscriptionRerankInOneCommand(t *testing.T) {
	isolateRerankConfig(t)
	vault := t.TempDir()
	writeNote(t, vault, "one.md", "---\nid: one\ntype: note\ntitle: One\nwhen: \"2026-09-10\"\n---\n# One\n")
	marker := fakeSubscriptionCLI(t, "codex")

	out, err := runCLI(t, installCmd(), vault, "--client", "codex", "--rerank-agent", "codex")
	if err != nil {
		t.Fatalf("install with rerank: %v\n%s", err, out)
	}
	if !strings.Contains(out, "enabled subscription rerank") || !strings.Contains(out, "gpt-5.6-luna") {
		t.Fatalf("one-command install omitted rerank receipt:\n%s", out)
	}
	sub, enabled, p, err := rerank.LoadLocalSubscription(vault)
	if err != nil {
		t.Fatal(err)
	}
	if !enabled || sub.Agent != "codex" || sub.Model != "gpt-5.6-luna" || sub.Policy != "auto" {
		t.Fatalf("installed local subscription = %#v enabled=%v", sub, enabled)
	}
	if strings.HasPrefix(p, vault) {
		t.Fatalf("subscription preference was written into the vault: %s", p)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("install executed the provider CLI")
	}
}
