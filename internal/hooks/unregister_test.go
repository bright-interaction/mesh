// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateHome points every client config that lives in the user's home at a temp
// dir, so a test never touches the developer's real Claude Desktop / Cursor / Codex
// configuration.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", home)         // windows path in clientConfig
	t.Setenv("XDG_CONFIG_HOME", home) // some linux resolvers consult it
	return home
}

// TestUnregisterMCPUndoesRegisterForEveryClient is the guard on the missing inverse.
// Mesh could register its MCP server into six clients' configs and had no way at all
// to remove it from any of them: `mesh hooks uninstall` printed a success receipt and
// left the entry behind, pointing at the ABSOLUTE path of a binary the user was about
// to delete, in a GLOBAL config next to all their other servers.
//
// It enumerates Clients rather than restating the list, so adding a seventh client
// without an uninstall path fails here instead of shipping.
func TestUnregisterMCPUndoesRegisterForEveryClient(t *testing.T) {
	for _, client := range Clients {
		t.Run(client, func(t *testing.T) {
			isolateHome(t)
			proj := t.TempDir()

			added, p, err := RegisterMCP(client, proj, "/v", "/abs/path/to/mesh")
			if err != nil || !added {
				t.Fatalf("RegisterMCP(%s) added=%v err=%v", client, added, err)
			}
			if reg, _, err := MCPRegistered(client, proj); err != nil || !reg {
				t.Fatalf("MCPRegistered(%s) after register = %v (err %v), want true", client, reg, err)
			}

			dropped, up, err := UnregisterMCP(client, proj)
			if err != nil {
				t.Fatalf("UnregisterMCP(%s): %v", client, err)
			}
			if !dropped {
				t.Fatalf("UnregisterMCP(%s) removed nothing; %s still holds the registration", client, p)
			}
			if reg, _, err := MCPRegistered(client, proj); err != nil || reg {
				t.Fatalf("MCPRegistered(%s) after unregister = %v (err %v), want false", client, reg, err)
			}
			data, _ := os.ReadFile(up)
			if strings.Contains(string(data), "/abs/path/to/mesh") {
				t.Errorf("%s still names the mesh binary after uninstall:\n%s", up, data)
			}

			// Idempotent, and re-installable afterwards.
			if again, _, err := UnregisterMCP(client, proj); err != nil || again {
				t.Errorf("second UnregisterMCP(%s) = %v (err %v), want false", client, again, err)
			}
			if re, _, err := RegisterMCP(client, proj, "/v", "/abs/path/to/mesh"); err != nil || !re {
				t.Errorf("re-register after uninstall(%s) = %v (err %v), want true", client, re, err)
			}
		})
	}
}

// TestUnregisterMCPKeepsEveryOtherServer is the blast-radius half: an uninstall that
// takes a user's other MCP servers with it is worse than no uninstall at all.
func TestUnregisterMCPKeepsEveryOtherServer(t *testing.T) {
	isolateHome(t)
	proj := t.TempDir()
	p := filepath.Join(proj, ".mcp.json")
	if err := os.WriteFile(p, []byte(`{"otherKey":1,"mcpServers":{"github":{"command":"gh-mcp"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RegisterMCP("claude-code", proj, "/v", "/bin/mesh"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := UnregisterMCP("claude-code", proj); err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	data, _ := os.ReadFile(p)
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("config is no longer valid JSON: %v\n%s", err, data)
	}
	servers, _ := cfg["mcpServers"].(map[string]any)
	if servers["github"] == nil {
		t.Errorf("uninstall dropped an unrelated MCP server:\n%s", data)
	}
	if _, gone := servers["mesh"]; gone {
		t.Errorf("mesh entry survived the uninstall:\n%s", data)
	}
	if cfg["otherKey"] == nil {
		t.Errorf("uninstall dropped an unrelated top-level key:\n%s", data)
	}
}

// TestUnregisterCodexTOMLKeepsTheRestOfTheFile covers the one format that is edited
// as text rather than parsed: the removal must take our table and nothing else.
func TestUnregisterCodexTOMLKeepsTheRestOfTheFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	original := "model = \"o4\"\n\n[mcp_servers.other]\ncommand = \"x\"\n"
	if err := os.WriteFile(p, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registerCodexTOML(p, "/v", "/bin/mesh"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := unregisterCodexTOML(p); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != original {
		t.Errorf("register then unregister must round-trip the file.\n got: %q\nwant: %q", got, original)
	}
}

// TestUnregisterCodexTOMLRemovesATableFollowedByAnother pins the boundary: the mesh
// table ends where the next table starts, so a table written after ours survives.
func TestUnregisterCodexTOMLRemovesATableFollowedByAnother(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	if _, _, err := registerCodexTOML(p, "/v", "/bin/mesh"); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("\n[mcp_servers.later]\ncommand = \"z\"\n")
	f.Close()

	if _, _, err := unregisterCodexTOML(p); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if strings.Contains(string(got), "mcp_servers.mesh") || strings.Contains(string(got), "/bin/mesh") {
		t.Errorf("mesh table survived:\n%s", got)
	}
	if !strings.Contains(string(got), "[mcp_servers.later]") || !strings.Contains(string(got), `command = "z"`) {
		t.Errorf("the table after ours was eaten:\n%s", got)
	}
}
