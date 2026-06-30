// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/hooks"
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
			// First session after `mesh install`: prepend a one-time welcome so the
			// agent greets the user and finishes onboarding. Only consume on the hook
			// path (the real SessionStart), never on a manual `mesh orient`.
			if hook && hooks.ConsumeOnboardPending(root) {
				text = onboardWelcome + text
			}
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

// onboardWelcome is injected (once) at the first SessionStart after `mesh install`,
// so the agent greets the user and finishes setup itself, no commands needed.
const onboardWelcome = `# First Mesh session - onboard the user now

The user just installed Mesh and this is their first session. Before their task:
1. Greet them warmly and say in one line what Mesh is: a knowledge mesh you (the agent) read at the start of every session and write back to at the end - the flywheel, so knowledge compounds instead of being relearned.
2. Tell them it is now wired to read automatically each session (this orientation IS that hook firing).
3. Ask two short questions: (a) enable write-back enforcement? - a one-time nudge per session to record decisions/gotchas, recommended; (b) want a 60-second tour of what is already in their mesh?
4. If they say yes to write-back, call mesh_setup_hooks with action=install. For a tour, use mesh_god_nodes + mesh_search.
Keep it short and friendly, then carry on with whatever they need.

---
`

// indexVault builds the initial index so the first orientation (and the agent) see
// real content immediately, instead of an empty mesh on the first session.
func indexVault(vaultAbs string) {
	if store, err := index.Open(vaultAbs); err == nil {
		if _, err := index.Reindex(store, vaultAbs); err == nil {
			if n, _ := store.Count("notes"); n > 0 {
				fmt.Printf("  + indexed the vault (%d notes)\n", n)
			}
		}
		store.Close()
	}
}

// installCmd is the one-shot agent setup. For Claude Code it registers the MCP
// server + the SessionStart read hook + arms the first-run welcome, so the agent
// onboards the user automatically. For other clients (which have no session hooks)
// it registers the MCP server in that client's own config and indexes the vault; the
// agent then uses Mesh via the MCP server's instructions.
func installCmd() *cobra.Command {
	var dir, client string
	var noMCP, enforce bool
	c := &cobra.Command{
		Use:   "install [vault]",
		Short: "One-shot setup: register the MCP server (and, on Claude Code, the auto-onboard hook)",
		Long:  "Wires a coding agent to use Mesh. --client claude-code (default) also installs the SessionStart read hook + a one-time welcome so the agent onboards you with no further commands. Other clients (claude-desktop, cursor, vscode, windsurf, codex) get the MCP server registered in their own config; session hooks are Claude Code only, so elsewhere the agent uses Mesh via the MCP server's instructions.",
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
			fmt.Printf("Setting up Mesh for %s...\n", client)

			if client == "claude-code" {
				if !noMCP {
					added, p, err := hooks.InstallMCP(projAbs, vaultAbs, bin)
					if err != nil {
						return err
					}
					if added {
						fmt.Printf("  + registered the Mesh MCP server in %s\n", p)
					} else {
						fmt.Printf("  . MCP server already present in %s\n", p)
					}
				}
				res, err := hooks.Install(hooks.Options{ProjectDir: projAbs, Vault: vaultAbs, Bin: bin, EnforceWriteback: enforce})
				if err != nil {
					return err
				}
				if len(res.Added) > 0 {
					for _, a := range res.Added {
						fmt.Printf("  + %s\n", a)
					}
				} else {
					fmt.Printf("  . session hooks already in %s\n", res.SettingsPath)
				}
				indexVault(vaultAbs)
				if err := hooks.SetOnboardPending(vaultAbs); err != nil {
					return err
				}
				fmt.Println("  + armed the first-run welcome")
				fmt.Println("\nDone. Start a new agent session (or reconnect the MCP server) and Mesh will")
				fmt.Println("greet you and finish onboarding automatically, no commands needed.")
				return nil
			}

			// Other clients: MCP only (no session hooks exist for them).
			added, p, err := hooks.RegisterMCP(client, projAbs, vaultAbs, bin)
			if err != nil {
				return err
			}
			if added {
				fmt.Printf("  + registered the Mesh MCP server for %s in %s\n", client, p)
			} else {
				fmt.Printf("  . MCP server already registered for %s in %s\n", client, p)
			}
			indexVault(vaultAbs)
			fmt.Printf("\nDone. Restart %s so it loads the MCP server.\n", client)
			fmt.Println("Note: the auto-onboard + write-back hooks are Claude Code only. Here the agent")
			fmt.Println("uses Mesh via the MCP server's instructions, just ask it to use Mesh, or run")
			fmt.Println("`mesh hooks install` if you also use Claude Code in this project.")
			return nil
		},
	}
	c.Flags().StringVar(&client, "client", "claude-code", "agent client: "+strings.Join(hooks.Clients, ", "))
	c.Flags().StringVar(&dir, "dir", ".", "project dir (used for claude-code .mcp.json/.claude and vscode .vscode)")
	c.Flags().BoolVar(&noMCP, "no-mcp", false, "skip registering the MCP server (claude-code only)")
	c.Flags().BoolVar(&enforce, "enforce-writeback", false, "also install the Stop write-back nudge now (claude-code; default: the agent asks during onboarding)")
	return c
}

// hooksCmd installs/removes the Claude Code session hooks that enforce the read-at-
// start / write-back-at-end discipline (the flywheel). These are SESSION hooks, not
// git pre/post-push hooks. The merge logic lives in internal/hooks, shared with the
// mesh_setup_hooks MCP onboarding tool.
func hooksCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "hooks",
		Short: "Set up Claude Code session hooks: read Mesh at session start, nudge write-back at end",
		Long:  "Installs Claude Code SessionStart + Stop hooks into a project's .claude/settings.json so an agent starts every session having read the mesh (SessionStart -> mesh orient) and is reminded once to write back what it learned before finishing (Stop -> mesh hooks stop-check). These are session-lifecycle hooks, not git pre/post-push hooks.",
	}
	c.AddCommand(hooksInstallCmd(), hooksUninstallCmd(), hooksStopCheckCmd())
	return c
}

func hooksInstallCmd() *cobra.Command {
	var dir string
	var readOnly, dryRun, autoExtract bool
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
			res, err := hooks.Install(hooks.Options{ProjectDir: projAbs, Vault: vaultAbs, Bin: bin, EnforceWriteback: !readOnly, AutoExtract: autoExtract, DryRun: dryRun})
			if err != nil {
				return err
			}
			if dryRun {
				fmt.Printf("# dry run: would write %s\n\n%s\n", res.SettingsPath, res.Preview)
				return nil
			}
			if len(res.Added) == 0 {
				fmt.Println("mesh hooks already installed (nothing to do).")
				return nil
			}
			fmt.Printf("installed into %s:\n", res.SettingsPath)
			for _, a := range res.Added {
				fmt.Printf("  + %s\n", a)
			}
			fmt.Println("\nRun /hooks in Claude Code to verify, then start a new session.")
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", ".", "project dir whose .claude/settings.json to edit")
	c.Flags().BoolVar(&readOnly, "read-only", false, "only the SessionStart read hook; skip the Stop write-back nudge")
	c.Flags().BoolVar(&autoExtract, "extract", false, "Stop hook auto-extracts session learnings into the review queue when the agent did not write back (spawns the BYOAI LLM per such session)")
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
			removed, p, err := hooks.Uninstall(projAbs)
			if err != nil {
				return err
			}
			fmt.Printf("removed %d mesh hook(s) from %s\n", removed, p)
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
	var vault string
	var autoExtract bool
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
				return nil // already wrote back: let it stop, nothing to extract
			}
			sid := in.SessionID
			if sid == "" {
				sid = "default"
			}
			marker := filepath.Join(os.TempDir(), "mesh-stop-"+sanitizeID(sid))
			if _, err := os.Stat(marker); err == nil {
				// Already nudged this session and the agent still has not written back.
				// As the fallback, auto-extract the session's learnings into the review
				// queue (once per session), if enabled. Never blocks the stop.
				if autoExtract && vault != "" && in.TranscriptPath != "" {
					exMarker := filepath.Join(os.TempDir(), "mesh-extracted-"+sanitizeID(sid))
					if _, err := os.Stat(exMarker); err != nil {
						_ = os.WriteFile(exMarker, []byte("1"), 0o644)
						spawnExtraction(vault, in.TranscriptPath)
					}
				}
				return nil // do not loop
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
	c.Flags().StringVar(&vault, "vault", "", "vault to queue auto-extracted candidates into (enables the fallback extractor)")
	c.Flags().BoolVar(&autoExtract, "extract", false, "auto-extract session learnings into the review queue when the agent did not write back")
	return c
}

// spawnExtraction launches `mesh extract --to-pending <vault> <transcript>` as a
// detached background process so the Stop hook returns immediately (the extraction
// runs the BYOAI LLM, which takes seconds). Best-effort: failures are logged to the
// vault's .mesh/extract.log, never surfaced to the hook's stdout (that is the hook
// protocol channel). Runs in its own process group so it outlives the hook.
func spawnExtraction(vault, transcript string) {
	self, err := os.Executable()
	if err != nil || self == "" {
		self = "mesh"
	}
	logPath := filepath.Join(vault, ".mesh", "extract.log")
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	lf, _ := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	cmd := exec.Command(self, "extract", "--to-pending", vault, transcript)
	if lf != nil {
		cmd.Stdout, cmd.Stderr = lf, lf
	}
	cmd.SysProcAttr = detachAttr() // detach from the hook's process group (platform-specific)
	if err := cmd.Start(); err != nil {
		if lf != nil {
			fmt.Fprintf(lf, "spawn extraction failed: %v\n", err)
		}
		return
	}
	_ = cmd.Process.Release() // do not wait; let it finish in the background
	if lf != nil {
		_ = lf.Close()
	}
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
