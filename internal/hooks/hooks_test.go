package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func opts(dir string, enforce bool) Options {
	return Options{ProjectDir: dir, Vault: "/v", Bin: "/bin/mesh", EnforceWriteback: enforce}
}

func TestInstallIdempotentAndUninstall(t *testing.T) {
	dir := t.TempDir()
	// pre-existing unrelated settings must be preserved.
	os.MkdirAll(filepath.Join(dir, ".claude"), 0o755)
	os.WriteFile(filepath.Join(dir, ".claude", "settings.json"), []byte(`{"model":"keepme"}`), 0o644)

	r, err := Install(opts(dir, true))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Added) != 2 {
		t.Fatalf("first install should add 2 hooks (read + write), got %d", len(r.Added))
	}
	st, _ := GetStatus(dir)
	if !st.Installed || !st.ReadHook || !st.WriteHook {
		t.Fatalf("status after install = %+v", st)
	}
	// unrelated key preserved.
	data, _ := os.ReadFile(st.SettingsPath)
	var m map[string]any
	json.Unmarshal(data, &m)
	if m["model"] != "keepme" {
		t.Error("unrelated settings key was dropped")
	}

	// idempotent: a second install adds nothing.
	r2, _ := Install(opts(dir, true))
	if len(r2.Added) != 0 {
		t.Errorf("re-install should add nothing, got %v", r2.Added)
	}

	removed, _, err := Uninstall(dir)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 3 { // 2 SessionStart groups + 1 Stop group
		t.Errorf("uninstall removed %d, want 3", removed)
	}
	st2, _ := GetStatus(dir)
	if st2.Installed || st2.ReadHook || st2.WriteHook {
		t.Errorf("status after uninstall = %+v", st2)
	}
}

func TestReadOnlyInstall(t *testing.T) {
	dir := t.TempDir()
	r, _ := Install(opts(dir, false)) // enforceWriteback=false
	if len(r.Added) != 1 {
		t.Fatalf("read-only install should add only the read hook, got %d", len(r.Added))
	}
	st, _ := GetStatus(dir)
	if !st.ReadHook || st.WriteHook {
		t.Errorf("read-only status = %+v (want read=true write=false)", st)
	}
}

func TestInstallMCPIdempotent(t *testing.T) {
	dir := t.TempDir()
	// pre-existing unrelated server must be preserved.
	os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte(`{"mcpServers":{"other":{"command":"x"}}}`), 0o644)
	added, p, err := InstallMCP(dir, "/v", "/bin/mesh")
	if err != nil || !added {
		t.Fatalf("first InstallMCP added=%v err=%v", added, err)
	}
	data, _ := os.ReadFile(p)
	var cfg map[string]any
	json.Unmarshal(data, &cfg)
	servers := cfg["mcpServers"].(map[string]any)
	if servers["other"] == nil {
		t.Error("existing mcp server was dropped")
	}
	if servers["mesh"] == nil {
		t.Error("mesh server was not registered")
	}
	added2, _, _ := InstallMCP(dir, "/v", "/bin/mesh")
	if added2 {
		t.Error("re-registering should be a no-op")
	}
}

func TestOnboardMarkerConsumeOnce(t *testing.T) {
	vault := t.TempDir()
	if ConsumeOnboardPending(vault) {
		t.Error("no marker yet: consume should be false")
	}
	if err := SetOnboardPending(vault); err != nil {
		t.Fatal(err)
	}
	if !ConsumeOnboardPending(vault) {
		t.Error("after SetOnboardPending, first consume should be true")
	}
	if ConsumeOnboardPending(vault) {
		t.Error("second consume should be false (fires exactly once)")
	}
}

func TestRegisterJSONServer(t *testing.T) {
	dir := t.TempDir()
	// mcpServers (Claude/Cursor/Windsurf), preserve an existing server.
	p := filepath.Join(dir, "mcp.json")
	os.WriteFile(p, []byte(`{"mcpServers":{"other":{"command":"x"}}}`), 0o644)
	added, _, err := registerJSONServer(p, "mcpServers", "/v", "/bin/mesh")
	if err != nil || !added {
		t.Fatalf("registerJSONServer added=%v err=%v", added, err)
	}
	var cfg map[string]any
	data, _ := os.ReadFile(p)
	json.Unmarshal(data, &cfg)
	servers := cfg["mcpServers"].(map[string]any)
	if servers["other"] == nil || servers["mesh"] == nil {
		t.Error("must add mesh and keep the existing server")
	}
	if a2, _, _ := registerJSONServer(p, "mcpServers", "/v", "/bin/mesh"); a2 {
		t.Error("re-register should be idempotent")
	}

	// VS Code uses the "servers" key and requires type:stdio.
	vp := filepath.Join(dir, "vscode.json")
	registerJSONServer(vp, "servers", "/v", "/bin/mesh")
	data, _ = os.ReadFile(vp)
	json.Unmarshal(data, &cfg)
	mesh := cfg["servers"].(map[string]any)["mesh"].(map[string]any)
	if mesh["type"] != "stdio" {
		t.Errorf("vscode server entry needs type:stdio, got %v", mesh["type"])
	}
}

func TestRegisterCodexTOML(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(p, []byte("model = \"o4\"\n"), 0o644)
	added, _, err := registerCodexTOML(p, "/v", "/bin/mesh")
	if err != nil || !added {
		t.Fatalf("codex add=%v err=%v", added, err)
	}
	data, _ := os.ReadFile(p)
	s := string(data)
	if !strings.Contains(s, "model = \"o4\"") || !strings.Contains(s, "[mcp_servers.mesh]") {
		t.Errorf("must preserve existing config and append the mesh table:\n%s", s)
	}
	if a2, _, _ := registerCodexTOML(p, "/v", "/bin/mesh"); a2 {
		t.Error("codex re-register should be idempotent")
	}
}

func TestClientConfigPaths(t *testing.T) {
	if _, f, _ := clientConfig("vscode", "/proj"); f != "servers" {
		t.Errorf("vscode format = %q, want servers", f)
	}
	if _, f, _ := clientConfig("codex", "/proj"); f != "toml" {
		t.Errorf("codex format = %q, want toml", f)
	}
	if _, _, err := clientConfig("nope", "/proj"); err == nil {
		t.Error("unknown client should error")
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	o := opts(dir, true)
	o.DryRun = true
	r, _ := Install(o)
	if r.Preview == "" || !strings.Contains(r.Preview, "SessionStart") {
		t.Error("dry run should return a preview containing the hooks")
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude", "settings.json")); err == nil {
		t.Error("dry run must not write the settings file")
	}
}
