package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/mcp"
	"github.com/spf13/cobra"
)

// orientCmd prints a session orientation: the most-connected entry points, what
// changed recently, and the retrieval contract. With --hook it emits the Claude
// Code SessionStart JSON envelope so a hook injects it as the session's first
// context, i.e. the agent literally starts having read the mesh.
func orientCmd() *cobra.Command {
	var hook bool
	var vaultFlag string
	c := &cobra.Command{
		Use:   "orient [vault]",
		Short: "Print a session orientation (entry points + recent changes + how to retrieve)",
		Long:  "Front-load an agent's session with the knowledge mesh. With --hook it emits the Claude Code SessionStart JSON envelope (hookSpecificOutput.additionalContext) so a SessionStart hook injects it automatically.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := vaultFlag
			if len(args) == 1 {
				root = args[0]
			}
			if root == "" {
				root = "."
			}
			store, err := index.Open(root)
			if err != nil {
				return err
			}
			defer store.Close()
			g, err := store.LoadGraph()
			if err != nil {
				return err
			}
			text := orientText(store, g)
			if hook {
				out, _ := json.Marshal(map[string]any{
					"hookSpecificOutput": map[string]any{
						"hookEventName":     "SessionStart",
						"additionalContext": text,
					},
				})
				fmt.Println(string(out))
				return nil
			}
			fmt.Print(text)
			return nil
		},
	}
	c.Flags().BoolVar(&hook, "hook", false, "emit the Claude Code SessionStart JSON envelope (additionalContext)")
	c.Flags().StringVar(&vaultFlag, "vault", "", "vault root (defaults to the positional arg or .)")
	return c
}

func orientText(store *index.Store, g *graph.Graph) string {
	var b strings.Builder
	b.WriteString("# Your knowledge mesh (read this first)\n\n")
	b.WriteString("A knowledge mesh is available via the mesh-* MCP tools. Orient with it before exploring files.\n\n")

	type hub struct {
		label, path string
		deg         int
	}
	var hubs []hub
	for _, n := range g.Nodes() {
		if n.Kind != "note" {
			continue
		}
		hubs = append(hubs, hub{n.Label, n.NotePath, n.Degree})
	}
	sort.Slice(hubs, func(i, j int) bool {
		if hubs[i].deg != hubs[j].deg {
			return hubs[i].deg > hubs[j].deg
		}
		return hubs[i].label < hubs[j].label
	})
	if len(hubs) > 10 {
		hubs = hubs[:10]
	}
	if len(hubs) > 0 {
		b.WriteString("## Entry points (most-connected notes)\n")
		for _, h := range hubs {
			b.WriteString(fmt.Sprintf("- %s (%s) [%d links]\n", h.label, h.path, h.deg))
		}
		b.WriteString("\n")
	}

	since := time.Now().Add(-7 * 24 * time.Hour).Unix()
	if refs, err := store.ChangedSince(since); err == nil && len(refs) > 0 {
		if len(refs) > 10 {
			refs = refs[:10]
		}
		b.WriteString("## Changed in the last 7 days\n")
		for _, r := range refs {
			b.WriteString(fmt.Sprintf("- %s (%s)\n", r.ID, r.Path))
		}
		b.WriteString("\n")
	}

	b.WriteString("## How to use it\n")
	b.WriteString(mcp.Contract())
	b.WriteString("\n")
	return b.String()
}

// hooksCmd installs/removes the Claude Code session hooks that force the read-at-
// start / write-back-at-end discipline. These are SESSION hooks (Claude Code), not
// git pre/post-push hooks: SessionStart injects the orientation, Stop nudges the
// write-back once. Git push hooks are a separate layer (the vault's post-commit
// reindex / the monorepo psync), unrelated to the agent session lifecycle.
func hooksCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "hooks",
		Short: "Set up Claude Code session hooks: read Mesh at session start, nudge write-back at end",
		Long:  "Installs Claude Code SessionStart + Stop hooks into a project's .claude/settings.json so an agent starts every session having read the mesh (SessionStart -> mesh orient) and is reminded once to write back what it learned before finishing (Stop -> mesh hooks stop-check). These are session-lifecycle hooks, not git pre/post-push hooks.",
	}
	c.AddCommand(hooksInstallCmd(), hooksUninstallCmd(), hooksStopCheckCmd())
	return c
}

func cmdEntry(command string) map[string]any { return map[string]any{"type": "command", "command": command} }

func appendHook(hooks map[string]any, event string, group map[string]any) {
	arr, _ := hooks[event].([]any)
	hooks[event] = append(arr, group)
}

// settingsHasCommand reports whether the settings already reference one of our hook
// commands, so install is idempotent.
func settingsHasCommand(settings map[string]any, substr string) bool {
	data, _ := json.Marshal(settings)
	return strings.Contains(string(data), substr)
}

func hooksInstallCmd() *cobra.Command {
	var dir string
	var readOnly, dryRun bool
	c := &cobra.Command{
		Use:   "install [vault]",
		Short: "Wire SessionStart (read Mesh) + Stop (nudge write-back) into .claude/settings.json",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			vaultPath := "."
			if len(args) == 1 {
				vaultPath = args[0]
			}
			vaultAbs, err := filepath.Abs(vaultPath)
			if err != nil {
				return err
			}
			projAbs, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			bin, err := os.Executable()
			if err != nil || bin == "" {
				bin = "mesh"
			}
			orientCommand := fmt.Sprintf("%q orient --hook --vault %q", bin, vaultAbs)
			stopCommand := fmt.Sprintf("%q hooks stop-check", bin)

			settingsPath := filepath.Join(projAbs, ".claude", "settings.json")
			settings := map[string]any{}
			if data, err := os.ReadFile(settingsPath); err == nil {
				if err := json.Unmarshal(data, &settings); err != nil {
					return fmt.Errorf("existing %s is not valid JSON: %w", settingsPath, err)
				}
			}
			hooks, _ := settings["hooks"].(map[string]any)
			if hooks == nil {
				hooks = map[string]any{}
			}

			var added []string
			if !settingsHasCommand(settings, "orient --hook") {
				appendHook(hooks, "SessionStart", map[string]any{"matcher": "startup", "hooks": []any{cmdEntry(orientCommand)}})
				appendHook(hooks, "SessionStart", map[string]any{"matcher": "resume", "hooks": []any{cmdEntry(orientCommand)}})
				added = append(added, "SessionStart -> mesh orient (the agent reads the mesh at session start)")
			}
			if !readOnly && !settingsHasCommand(settings, "hooks stop-check") {
				appendHook(hooks, "Stop", map[string]any{"hooks": []any{cmdEntry(stopCommand)}})
				added = append(added, "Stop -> mesh hooks stop-check (nudges write-back once before finishing)")
			}
			settings["hooks"] = hooks

			out, _ := json.MarshalIndent(settings, "", "  ")
			if dryRun {
				fmt.Printf("# dry run: would write %s\n\n%s\n", settingsPath, string(out))
				return nil
			}
			if len(added) == 0 {
				fmt.Println("mesh hooks already installed (nothing to do).")
				return nil
			}
			if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(settingsPath, append(out, '\n'), 0o644); err != nil {
				return err
			}
			fmt.Printf("installed into %s:\n", settingsPath)
			for _, a := range added {
				fmt.Printf("  + %s\n", a)
			}
			fmt.Println("\nRun /hooks in Claude Code to verify, then start a new session.")
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", ".", "project dir whose .claude/settings.json to edit")
	c.Flags().BoolVar(&readOnly, "read-only", false, "only the SessionStart read hook; skip the Stop write-back nudge")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be written without changing anything")
	return c
}

func hooksUninstallCmd() *cobra.Command {
	var dir string
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the mesh session hooks from .claude/settings.json",
		RunE: func(cmd *cobra.Command, args []string) error {
			projAbs, _ := filepath.Abs(dir)
			settingsPath := filepath.Join(projAbs, ".claude", "settings.json")
			data, err := os.ReadFile(settingsPath)
			if err != nil {
				return fmt.Errorf("no settings at %s", settingsPath)
			}
			settings := map[string]any{}
			if err := json.Unmarshal(data, &settings); err != nil {
				return err
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
			if err := os.WriteFile(settingsPath, append(out, '\n'), 0o644); err != nil {
				return err
			}
			fmt.Printf("removed %d mesh hook(s) from %s\n", removed, settingsPath)
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", ".", "project dir")
	return c
}

// hooksStopCheckCmd is the Stop hook itself: if the agent already wrote back this
// session it allows the stop; otherwise it blocks ONCE (a per-session marker) with
// a reminder to call mesh_append_note, so the discipline is enforced without an
// infinite loop.
func hooksStopCheckCmd() *cobra.Command {
	c := &cobra.Command{
		Use:    "stop-check",
		Short:  "Internal: a Stop hook that nudges write-back to Mesh once per session",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var in struct {
				SessionID      string `json:"session_id"`
				TranscriptPath string `json:"transcript_path"`
			}
			data, _ := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
			_ = json.Unmarshal(data, &in)

			if in.TranscriptPath != "" && transcriptHasWriteback(in.TranscriptPath) {
				return nil // already wrote back: let it stop
			}
			sid := in.SessionID
			if sid == "" {
				sid = "default"
			}
			marker := filepath.Join(os.TempDir(), "mesh-stop-"+sanitizeID(sid))
			if _, err := os.Stat(marker); err == nil {
				return nil // already nudged this session: do not loop
			}
			_ = os.WriteFile(marker, []byte("1"), 0o644)
			out, _ := json.Marshal(map[string]any{
				"decision": "block",
				"reason":   "Mesh: record what you learned before finishing (keeps the next session smart).",
				"hookSpecificOutput": map[string]any{
					"hookEventName":     "Stop",
					"additionalContext": "Before you finish: if this session produced any non-obvious decision, gotcha, or post-mortem, call mesh_append_note (type decision|gotcha|post-mortem with a one-line do/dont/why), or mesh_write_entity for a new system/tool/concept, so the next agent inherits it. If there is genuinely nothing worth recording, you may stop.",
				},
			})
			fmt.Println(string(out))
			return nil
		},
	}
	return c
}

// transcriptHasWriteback scans a session transcript for an actual mesh write TOOL
// CALL (the quoted tool name in a tool_use entry), not a mere mention of it in the
// injected contract text.
func transcriptHasWriteback(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(line, `"mesh_append_note"`) || strings.Contains(line, `"mesh_write_entity"`) {
			return true
		}
	}
	return false
}

func sanitizeID(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, s)
}
