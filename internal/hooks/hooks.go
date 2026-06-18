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
	"strings"
)

// Options controls an install.
type Options struct {
	ProjectDir       string // project whose .claude/settings.json to edit
	Vault            string // absolute vault path the agent should orient from
	Bin              string // absolute mesh binary path the hooks invoke
	EnforceWriteback bool   // also add the Stop write-back nudge
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

func settingsPath(projectDir string) string { return filepath.Join(projectDir, ".claude", "settings.json") }
func orientCommand(bin, vault string) string {
	return fmt.Sprintf("%q orient --hook --vault %q", bin, vault)
}
func stopCommand(bin string) string { return fmt.Sprintf("%q hooks stop-check", bin) }

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

func cmdEntry(command string) map[string]any { return map[string]any{"type": "command", "command": command} }
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
	if o.EnforceWriteback && !has(settings, "hooks stop-check") {
		appendHook(hooks, "Stop", map[string]any{"hooks": []any{cmdEntry(stopCommand(o.Bin))}})
		res.Added = append(res.Added, "Stop -> mesh hooks stop-check (nudges write-back once before finishing)")
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
