// Package hooks installs the Claude Code SESSION hooks that make an agent use Mesh
// automatically: read the mesh at session start, and get nudged to write back what
// it learned before finishing. Shared by the `mesh hooks` CLI and the
// mesh_setup_hooks MCP onboarding tool so both write the exact same config.
//
// These are Claude Code session-lifecycle hooks (.claude/settings.json), NOT git
// pre/post-push hooks. SessionStart injects an orientation; Stop nudges write-back
// once per session. SessionEnd cannot enforce anything (cleanup only), so Stop is
// the enforcement point.
package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Clients lists the agent clients mesh install can register the MCP server for.
// claude-code is the only one that also supports session hooks (auto-onboard);
// the rest get the MCP tools + the server's instructions nudge.
var Clients = []string{"claude-code", "claude-desktop", "cursor", "vscode", "windsurf", "codex"}

// clientConfig returns the MCP config path + format for a client. Formats:
// "mcpServers" (Claude Desktop/Code/Cursor/Windsurf JSON), "servers" (VS Code JSON,
// needs type:stdio), "toml" (Codex ~/.codex/config.toml).
func clientConfig(client, projectDir string) (path, format string, err error) {
	home, _ := os.UserHomeDir()
	switch client {
	case "claude-code":
		return filepath.Join(projectDir, ".mcp.json"), "mcpServers", nil
	case "claude-desktop":
		switch runtime.GOOS {
		case "darwin":
			return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json"), "mcpServers", nil
		case "windows":
			return filepath.Join(os.Getenv("APPDATA"), "Claude", "claude_desktop_config.json"), "mcpServers", nil
		default:
			return filepath.Join(home, ".config", "Claude", "claude_desktop_config.json"), "mcpServers", nil
		}
	case "cursor":
		return filepath.Join(home, ".cursor", "mcp.json"), "mcpServers", nil
	case "windsurf":
		return filepath.Join(home, ".codeium", "windsurf", "mcp_config.json"), "mcpServers", nil
	case "vscode":
		return filepath.Join(projectDir, ".vscode", "mcp.json"), "servers", nil
	case "codex":
		return filepath.Join(home, ".codex", "config.toml"), "toml", nil
	default:
		return "", "", fmt.Errorf("unknown client %q (use one of: %s)", client, strings.Join(Clients, ", "))
	}
}

// RegisterMCP registers the Mesh MCP server in the given client's config, in that
// client's own format and location. Idempotent; preserves other servers.
func RegisterMCP(client, projectDir, vaultAbs, binPath string) (bool, string, error) {
	p, format, err := clientConfig(client, projectDir)
	if err != nil {
		return false, "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return false, p, err
	}
	if format == "toml" {
		return registerCodexTOML(p, vaultAbs, binPath)
	}
	return registerJSONServer(p, format, vaultAbs, binPath)
}

func registerJSONServer(p, key, vaultAbs, binPath string) (bool, string, error) {
	cfg := map[string]any{}
	if data, err := os.ReadFile(p); err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return false, p, fmt.Errorf("existing %s is not valid JSON: %w", p, err)
		}
	}
	servers, _ := cfg[key].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	if _, exists := servers["mesh"]; exists {
		return false, p, nil
	}
	entry := map[string]any{"command": binPath, "args": []any{"mcp", "--vault", vaultAbs, "--watch"}}
	if key == "servers" { // VS Code requires an explicit transport type
		entry["type"] = "stdio"
	}
	servers["mesh"] = entry
	cfg[key] = servers
	out, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(p, append(out, '\n'), 0o644); err != nil {
		return false, p, err
	}
	return true, p, nil
}

// registerCodexTOML appends a [mcp_servers.mesh] table to Codex's config.toml.
// Appending is safe TOML and idempotent (skip if the table already exists), which
// avoids needing a TOML parser to merge.
func registerCodexTOML(p, vaultAbs, binPath string) (bool, string, error) {
	if data, err := os.ReadFile(p); err == nil && strings.Contains(string(data), "[mcp_servers.mesh]") {
		return false, p, nil
	}
	block := fmt.Sprintf("\n[mcp_servers.mesh]\ncommand = %q\nargs = [\"mcp\", \"--vault\", %q, \"--watch\"]\n", binPath, vaultAbs)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return false, p, err
	}
	defer f.Close()
	if _, err := f.WriteString(block); err != nil {
		return false, p, err
	}
	return true, p, nil
}

// Options controls an install.
type Options struct {
	ProjectDir       string // project whose .claude/settings.json to edit
	Vault            string // absolute vault path the agent should orient from
	Bin              string // absolute mesh binary path the hooks invoke
	EnforceWriteback bool   // also add the Stop write-back nudge
	AutoExtract      bool   // Stop hook also auto-extracts candidates when the agent did not write back (spawns the BYOAI LLM per such session)
	DryRun           bool   // do not write; return the would-be settings in Preview
}

// Result describes what an install did.
type Result struct {
	SettingsPath string   `json:"settings_path"`
	Added        []string `json:"added"`
	Preview      string   `json:"preview,omitempty"` // set on DryRun
}

// Status reports whether the hooks are present in a project.
type Status struct {
	SettingsPath string `json:"settings_path"`
	Installed    bool   `json:"installed"`
	ReadHook     bool   `json:"read_hook"`
	WriteHook    bool   `json:"write_hook"`
}

func settingsPath(projectDir string) string {
	return filepath.Join(projectDir, ".claude", "settings.json")
}
func orientCommand(bin, vault string) string {
	return fmt.Sprintf("%q orient --hook --vault %q", bin, vault)
}
func stopCommand(bin, vault string, autoExtract bool) string {
	s := fmt.Sprintf("%q hooks stop-check", bin)
	if autoExtract {
		// Auto-extract the session's learnings into the review queue when the agent
		// did not write back itself. Spawns the BYOAI LLM (claude -p) per such session.
		s += fmt.Sprintf(" --vault %q --extract", vault)
	}
	return s
}

func load(projectDir string) (map[string]any, string, error) {
	p := settingsPath(projectDir)
	s := map[string]any{}
	if data, err := os.ReadFile(p); err == nil {
		if err := json.Unmarshal(data, &s); err != nil {
			return nil, p, fmt.Errorf("existing %s is not valid JSON: %w", p, err)
		}
	}
	return s, p, nil
}

func cmdEntry(command string) map[string]any {
	return map[string]any{"type": "command", "command": command}
}
func appendHook(hooks map[string]any, event string, group map[string]any) {
	arr, _ := hooks[event].([]any)
	hooks[event] = append(arr, group)
}

// has reports whether the settings already reference one of our hook commands, so
// installs are idempotent.
func has(settings map[string]any, substr string) bool {
	data, _ := json.Marshal(settings)
	return strings.Contains(string(data), substr)
}

// upgradeStopExtract rewrites an existing nudge-only Stop hook command
// (`mesh hooks stop-check`) to the auto-extract variant in place, so re-running
// `mesh hooks install --extract` turns extraction on for a project that already had
// the plain nudge. Returns whether it changed a command. It mutates the maps under
// settings["hooks"], so the caller's marshal of settings reflects the change.
func upgradeStopExtract(hooks map[string]any, bin, vault string) bool {
	groups, _ := hooks["Stop"].([]any)
	want := stopCommand(bin, vault, true)
	changed := false
	for _, g := range groups {
		gm, _ := g.(map[string]any)
		if gm == nil {
			continue
		}
		entries, _ := gm["hooks"].([]any)
		for _, e := range entries {
			em, _ := e.(map[string]any)
			if em == nil {
				continue
			}
			if cmd, _ := em["command"].(string); strings.Contains(cmd, "hooks stop-check") && !strings.Contains(cmd, "--extract") {
				em["command"] = want
				changed = true
			}
		}
	}
	return changed
}

// Install merges the Mesh session hooks into the project's .claude/settings.json,
// preserving any existing settings/hooks. Idempotent.
func Install(o Options) (Result, error) {
	var res Result
	settings, p, err := load(o.ProjectDir)
	if err != nil {
		return res, err
	}
	res.SettingsPath = p
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	oc := orientCommand(o.Bin, o.Vault)
	if !has(settings, "orient --hook") {
		appendHook(hooks, "SessionStart", map[string]any{"matcher": "startup", "hooks": []any{cmdEntry(oc)}})
		appendHook(hooks, "SessionStart", map[string]any{"matcher": "resume", "hooks": []any{cmdEntry(oc)}})
		res.Added = append(res.Added, "SessionStart -> mesh orient (the agent reads the mesh at session start)")
	}
	switch {
	case o.EnforceWriteback && !has(settings, "hooks stop-check"):
		appendHook(hooks, "Stop", map[string]any{"hooks": []any{cmdEntry(stopCommand(o.Bin, o.Vault, o.AutoExtract))}})
		msg := "Stop -> mesh hooks stop-check (nudges write-back once before finishing)"
		if o.AutoExtract {
			msg = "Stop -> mesh hooks stop-check --extract (nudges write-back, then auto-extracts candidates for review if the agent did not)"
		}
		res.Added = append(res.Added, msg)
	case o.AutoExtract && has(settings, "hooks stop-check") && !has(settings, "--extract"):
		// Upgrade path: the project already has the plain write-back nudge, so the
		// bare-substring guard above would skip it and leave --extract silently off.
		// Rewrite the existing Stop command in place to turn auto-extraction on.
		if upgradeStopExtract(hooks, o.Bin, o.Vault) {
			res.Added = append(res.Added, "Stop -> mesh hooks stop-check --extract (upgraded the existing write-back nudge to also auto-extract review candidates)")
		}
	}
	settings["hooks"] = hooks
	out, _ := json.MarshalIndent(settings, "", "  ")
	if o.DryRun {
		res.Preview = string(out)
		return res, nil
	}
	if len(res.Added) == 0 {
		return res, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return res, err
	}
	if err := os.WriteFile(p, append(out, '\n'), 0o644); err != nil {
		return res, err
	}
	return res, nil
}

// Uninstall removes the Mesh hook entries, leaving the rest of the settings intact.
func Uninstall(projectDir string) (int, string, error) {
	p := settingsPath(projectDir)
	if _, err := os.Stat(p); err != nil {
		return 0, p, fmt.Errorf("no settings at %s", p)
	}
	settings, _, err := load(projectDir)
	if err != nil {
		return 0, p, err
	}
	hooks, _ := settings["hooks"].(map[string]any)
	removed := 0
	for event, v := range hooks {
		arr, _ := v.([]any)
		kept := make([]any, 0, len(arr))
		for _, g := range arr {
			gb, _ := json.Marshal(g)
			if strings.Contains(string(gb), "orient --hook") || strings.Contains(string(gb), "hooks stop-check") {
				removed++
				continue
			}
			kept = append(kept, g)
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		settings["hooks"] = hooks
	}
	out, _ := json.MarshalIndent(settings, "", "  ")
	if err := os.WriteFile(p, append(out, '\n'), 0o644); err != nil {
		return removed, p, err
	}
	return removed, p, nil
}

// InstallMCP registers the Mesh MCP server in the project's .mcp.json so the agent
// gets the mesh-* tools without any manual config. Idempotent; preserves other
// servers. Returns whether it added the entry.
func InstallMCP(projectDir, vaultAbs, binPath string) (bool, string, error) {
	p := filepath.Join(projectDir, ".mcp.json")
	cfg := map[string]any{}
	if data, err := os.ReadFile(p); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return false, p, fmt.Errorf("existing %s is not valid JSON: %w", p, err)
		}
	}
	servers, _ := cfg["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	if _, exists := servers["mesh"]; exists {
		return false, p, nil
	}
	servers["mesh"] = map[string]any{
		"command": binPath,
		"args":    []any{"mcp", "--vault", vaultAbs, "--watch"},
	}
	cfg["mcpServers"] = servers
	out, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return false, p, err
	}
	if err := os.WriteFile(p, append(out, '\n'), 0o644); err != nil {
		return false, p, err
	}
	return true, p, nil
}

func onboardMarker(vaultRoot string) string { return filepath.Join(vaultRoot, ".mesh", "onboard") }

// SetOnboardPending arms a one-time welcome: the next SessionStart orient prepends an
// onboarding instruction so the agent greets the user and finishes setup itself.
func SetOnboardPending(vaultRoot string) error {
	p := onboardMarker(vaultRoot)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte("1"), 0o644)
}

// ConsumeOnboardPending returns true at most once (clearing the marker), so the
// welcome fires on exactly the first session after install.
func ConsumeOnboardPending(vaultRoot string) bool {
	p := onboardMarker(vaultRoot)
	if _, err := os.Stat(p); err != nil {
		return false
	}
	_ = os.Remove(p)
	return true
}

// GetStatus reports whether the hooks are installed in a project.
func GetStatus(projectDir string) (Status, error) {
	settings, p, err := load(projectDir)
	st := Status{SettingsPath: p}
	if err != nil {
		return st, err
	}
	st.ReadHook = has(settings, "orient --hook")
	st.WriteHook = has(settings, "hooks stop-check")
	st.Installed = st.ReadHook
	return st, nil
}
