// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/hooks"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/relate"
	"github.com/bright-interaction/mesh/internal/retrieve"
	"github.com/bright-interaction/mesh/internal/vault"
)

func obj(m map[string]any) map[string]any { return m }

func (s *Server) handleToolsList() any { return map[string]any{"tools": ToolSpecs()} }

// ToolSpecs returns the MCP tool definitions (name, description, inputSchema). It
// is the single source for both the MCP tools/list response and the web app's API
// reference, so the two never drift.
func ToolSpecs() []map[string]any {
	str := map[string]any{"type": "string"}
	intp := map[string]any{"type": "integer"}
	strList := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}

	tools := []map[string]any{
		{
			"name":        "mesh_search",
			"description": "Fused retrieval over the vault (full-text + graph proximity), tier-0 boosted (decisions/gotchas/post-mortems first). Returns ranked cards. Pass a token budget to get the best bundle that fits (default 8000). limit defaults to 20 and is capped at 100: narrow the query rather than raising it. Start here.",
			"inputSchema": obj(map[string]any{
				"type":       "object",
				"required":   []string{"query"},
				"properties": map[string]any{"query": str, "budget": intp, "limit": intp},
			}),
		},
		{
			"name":        "mesh_fetch",
			"description": "Fetch a note's full markdown by id (optionally just one heading section via anchor). Only call this when a search card is not enough.",
			"inputSchema": obj(map[string]any{
				"type":       "object",
				"required":   []string{"id"},
				"properties": map[string]any{"id": str, "anchor": str},
			}),
		},
		{
			"name":        "mesh_god_nodes",
			"description": "The map: the most-connected notes (hubs), best entry points to orient before searching.",
			"inputSchema": obj(map[string]any{"type": "object", "properties": map[string]any{"limit": intp}}),
		},
		{
			"name":        "mesh_changed_since",
			"description": "Notes modified after a unix timestamp, newest first. Pull only deltas when resuming. `since` is UNIX SECONDS, not milliseconds: a millisecond stamp is refused rather than silently matching nothing. limit defaults to 100 and is capped at 500; a capped reply sets truncated=true, so an absent truncated flag means you have the complete delta.",
			"inputSchema": obj(map[string]any{"type": "object", "required": []string{"since"}, "properties": map[string]any{"since": intp, "limit": intp}}),
		},
		{
			"name":        "mesh_neighbors",
			"description": "The typed neighborhood of a note (its linked notes/tags, in and out), out to a small depth. Walk the graph one hop at a time instead of fetching whole files.",
			"inputSchema": obj(map[string]any{
				"type":       "object",
				"required":   []string{"id"},
				"properties": map[string]any{"id": str, "depth": intp, "limit": intp},
			}),
		},
		{
			"name":        "mesh_community",
			"description": "With an id: the note's community and its members. Without: the community overview (clusters by size with an exemplar each) to orient before searching.",
			"inputSchema": obj(map[string]any{"type": "object", "properties": map[string]any{"id": str, "limit": intp}}),
		},
		{
			"name":        "mesh_append_note",
			"description": "Write back what you learned: create a decision/gotcha/post-mortem/note with do/dont/why so the next agent inherits it (the flywheel). Mesh fills id/timestamp/placement.",
			"inputSchema": obj(map[string]any{
				"type":     "object",
				"required": []string{"type", "title"},
				"properties": map[string]any{
					"type": str, "title": str, "do": str, "dont": str, "why": str,
					"related": strList, "tags": strList, "status": str, "severity": str,
					// Optional provenance. Mesh stamps the calling agent + source=agent
					// automatically; override author/confidence/review_by when you know them.
					"author": str, "source": str, "source_url": str, "confidence": str, "review_by": str,
				},
			}),
		},
		{
			"name":        "mesh_write_entity",
			"description": "Create an entity note (a system, tool, or concept page) with related links.",
			"inputSchema": obj(map[string]any{
				"type":       "object",
				"required":   []string{"title"},
				"properties": map[string]any{"title": str, "why": str, "related": strList, "tags": strList},
			}),
		},
		{
			"name":        "mesh_reindex",
			"description": "Re-read the vault from disk and rebuild the index NOW. Call this right after you edit note files directly in the editor or CLI so your next mesh_search/mesh_fetch/mesh_neighbors reflects the edits with no watcher lag (works even when the server was started without --watch). Returns what changed.",
			"inputSchema": obj(map[string]any{"type": "object", "properties": map[string]any{}}),
		},
		{
			"name":        "mesh_health",
			"description": "Run the knowledge-lifecycle health check NOW and return what is rotting: notes that cite a source file no longer in the code index (dead_ref), notes past their review_by date (overdue), plus any contradiction findings. Use this to keep the vault trustworthy; fix or update the flagged notes. Returns findings grouped by issue + counts.",
			"inputSchema": obj(map[string]any{"type": "object", "properties": map[string]any{"issue": str}}),
		},
		{
			"name":        "mesh_code_search",
			"description": "Locate SOURCE-CODE symbols (functions, types, methods, classes) by name across the indexed repos, ranked by name match. Returns cards with a file:line locator and signature so you jump straight to a definition instead of grepping the tree. Use this for 'where is X defined / what's in this area of the code'. This is the code index; mesh_search is for notes/knowledge. limit defaults to 12 and is capped at 100: narrow the query rather than raising it.",
			"inputSchema": obj(map[string]any{
				"type":       "object",
				"required":   []string{"query"},
				"properties": map[string]any{"query": str, "limit": intp, "languages": strList},
			}),
		},
		{
			"name":        "mesh_code_neighbors",
			"description": "The call-graph neighborhood of a code symbol by id (an id from mesh_code_search): callees (what it calls) and callers (what calls it). Go has full edges; other languages return symbol locations without a call graph.",
			"inputSchema": obj(map[string]any{
				"type":       "object",
				"required":   []string{"id"},
				"properties": map[string]any{"id": str},
			}),
		},
		{
			"name":        "mesh_code_context",
			"description": "What do we KNOW about this code: resolve a symbol by name (like mesh_code_search) and return each match together with the team's notes that reference it (decisions/gotchas/post-mortems about that function or type). Use this before changing a function to inherit the institutional knowledge around it, not just its signature.",
			"inputSchema": obj(map[string]any{
				"type":       "object",
				"required":   []string{"query"},
				"properties": map[string]any{"query": str, "limit": intp},
			}),
		},
		{
			"name":        "mesh_secret_status",
			"description": "Is a secret vault attached? Reports whether Mesh is wired to a Dockyard capability-mode vault (encrypted secret storage + autorotation) and how to use it. No network call, never echoes a key. Call this to discover the broker before mesh_secret_list / mesh_secret_use.",
			"inputSchema": obj(map[string]any{"type": "object", "properties": map[string]any{}}),
		},
		{
			"name":        "mesh_secret_list",
			"description": "List the secrets in the attached vault (NAMES + provider + rotation status only, NEVER a value). Use it to see what credentials you can broker before calling mesh_secret_use. Returns an empty note if no vault is attached.",
			"inputSchema": obj(map[string]any{"type": "object", "properties": map[string]any{}}),
		},
		{
			"name":        "mesh_secret_use",
			"description": "Get a credential you can use WITHOUT ever seeing it. Give the destination you will call (host+path, e.g. 'api.openai.com/v1/chat/completions'); Mesh mints a short-lived, single-use capability TOKEN bound to that destination. Send your request to the returned proxy_url with header `Authorization: Capability <token>` and the Dockyard vault injects the real, auto-rotated secret server-side. The plaintext secret never reaches you. Optionally pass secret_name to pin a specific vault entry and method to bind the HTTP method. Mint a fresh token per request; never store or log it.",
			"inputSchema": obj(map[string]any{
				"type":     "object",
				"required": []string{"destination"},
				"properties": map[string]any{
					"destination": str, "secret_name": str, "method": str, "ttl_seconds": intp,
				},
			}),
		},
		{
			"name":        "mesh_setup_hooks",
			"description": "ONBOARDING. Set up Claude Code SESSION hooks so you (the agent) automatically READ the mesh at the start of every session and get nudged to WRITE BACK what you learned before finishing - the flywheel that makes Mesh compound. Call with no args (action=status) to get the current install state plus the pitch and the exact questions to ask the user; then call action=install (read_only to skip the write-back nudge; dry_run to preview the settings.json first) to wire it into the project's .claude/settings.json. Offer this the FIRST time a user connects to Mesh. These are session hooks, NOT git pre/post-push.",
			"inputSchema": obj(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action":      map[string]any{"type": "string", "enum": []string{"status", "install", "uninstall"}},
					"project_dir": str,
					"read_only":   map[string]any{"type": "boolean"},
					"dry_run":     map[string]any{"type": "boolean"},
				},
			}),
		},
	}
	return tools
}

// Contract returns the agent-usage contract text (how to retrieve cheaply), shared
// by the MCP initialize instructions and the web app's API reference.
func Contract() string { return contractText }

// ToolNames returns just the tool names in ToolSpecs() order. The mesh://capabilities
// resource uses it so its advertised tool list can never drift from the real surface
// (it used to hardcode a slice that silently went stale on every new tool).
func ToolNames() []string {
	specs := ToolSpecs()
	names := make([]string, 0, len(specs))
	for _, t := range specs {
		if n, ok := t["name"].(string); ok {
			names = append(names, n)
		}
	}
	return names
}

// toolClass is how a tool relates to scope-RBAC. It is the choke point that stops the
// recurring "a new read tool leaks across scopes by default" bug (changed_since /
// health / code_* each shipped that way and were patched one at a time). EVERY tool
// must be classified here: TestEveryToolIsScopeClassified fails at build time if a tool
// in ToolSpecs()/the dispatch has no class, and handleToolsCall refuses an unclassified
// tool at runtime (fail closed). The per-handler filtering still does the fine-grained
// work; this map forces a conscious scope decision for every tool that ships.
type toolClass int

const (
	// classFiltered: a read tool that returns only the caller's readable SUBSET (it
	// consults scopeFromCtx and filters per note / opaque-404s an out-of-scope id).
	classFiltered toolClass = iota
	// classCodeDev: reads the code index, which carries no per-note scope and is treated
	// as dev-scoped in whole. The handler denies the ENTIRE call when the caller cannot
	// read the dev scope (codeScopeDenied).
	classCodeDev
	// classWrite: creates a note; the handler write-gates and stamps the caller's scope.
	classWrite
	// classOpen: no vault content crosses a scope boundary (a local operator action).
	classOpen
)

// toolScopeClass MUST contain every tool name in ToolSpecs(). Add a new tool here at
// the same time you add it to ToolSpecs() and the dispatch, having decided how it
// relates to scope. The test + the runtime check below both fail closed otherwise.
var toolScopeClass = map[string]toolClass{
	"mesh_search":         classFiltered,
	"mesh_fetch":          classFiltered,
	"mesh_god_nodes":      classFiltered,
	"mesh_changed_since":  classFiltered,
	"mesh_neighbors":      classFiltered,
	"mesh_community":      classFiltered,
	"mesh_reindex":        classFiltered,
	"mesh_health":         classFiltered,
	"mesh_append_note":    classWrite,
	"mesh_write_entity":   classWrite,
	"mesh_code_search":    classCodeDev,
	"mesh_code_neighbors": classCodeDev,
	"mesh_code_context":   classCodeDev,
	"mesh_setup_hooks":    classOpen,
	// Secret-broker tools broker an ATTACHED Dockyard vault, not vault-note content, so
	// no per-note scope crosses a boundary (classOpen). All three apply the write-role
	// gate inside their handler: minting a token spends the team's credential, and
	// listing or status disclose the vault inventory and the broker endpoint, none of
	// which a read-only hosted viewer should reach.
	"mesh_secret_status": classOpen,
	"mesh_secret_list":   classOpen,
	"mesh_secret_use":    classOpen,
}

func (s *Server) handleToolsCall(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "bad params"}
	}
	// Fail closed on any tool that was never scope-classified: a dispatched tool missing
	// from toolScopeClass must not run, so a new tool cannot silently bypass the gate.
	if _, classified := toolScopeClass[p.Name]; !classified {
		return nil, &rpcError{Code: codeMethodNotFound, Message: "unknown tool", Data: p.Name}
	}
	switch p.Name {
	case "mesh_search":
		return s.toolSearch(ctx, p.Arguments)
	case "mesh_fetch":
		return s.toolFetch(ctx, p.Arguments)
	case "mesh_god_nodes":
		return s.toolGodNodes(ctx, p.Arguments)
	case "mesh_changed_since":
		return s.toolChangedSince(ctx, p.Arguments)
	case "mesh_neighbors":
		return s.toolNeighbors(ctx, p.Arguments)
	case "mesh_community":
		return s.toolCommunity(ctx, p.Arguments)
	case "mesh_append_note":
		return s.toolWrite(ctx, p.Arguments, "")
	case "mesh_write_entity":
		return s.toolWrite(ctx, p.Arguments, "entity")
	case "mesh_reindex":
		return s.toolReindex(ctx)
	case "mesh_health":
		return s.toolHealth(ctx, p.Arguments)
	case "mesh_code_search":
		return s.toolCodeSearch(ctx, p.Arguments)
	case "mesh_code_context":
		return s.toolCodeContext(ctx, p.Arguments)
	case "mesh_code_neighbors":
		return s.toolCodeNeighbors(ctx, p.Arguments)
	case "mesh_secret_status":
		return s.toolSecretStatus(ctx)
	case "mesh_secret_list":
		return s.toolSecretList(ctx, p.Arguments)
	case "mesh_secret_use":
		return s.toolSecretUse(ctx, p.Arguments)
	case "mesh_setup_hooks":
		return s.toolSetupHooks(ctx, p.Arguments)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "unknown tool", Data: p.Name}
	}
}

// toolReindex forces an authoritative reconcile so an agent editing note files
// directly (via the editor or CLI) can make its edits queryable on demand, instead
// of waiting on the --watch debounce or restarting a no-watch server. Authoritative
// = full content-hash check, so it also catches an edit that did not move the mtime.
func (s *Server) toolReindex(ctx context.Context) (any, *rpcError) {
	// Role write gate: a reindex content-hashes the whole vault, re-indexes the code
	// roots, and WRITES the result to the DB, so it is not a read even though what it
	// returns is. A read-only hosted viewer must not be able to drive it.
	if can, set := writeAllowed(ctx); set && !can {
		return nil, &rpcError{Code: codeInvalidParams, Message: "forbidden: your role is read-only"}
	}
	// Throttle remote callers only (see reconcileThrottled): the hosted transports are
	// where back-to-back passes contend with the hub's post-sync reconcile worker for
	// reloadMu. The local operator's "reindex now" still means now.
	rec, throttled, err := s.reconcileThrottled(!localOperator(ctx))
	if err != nil {
		return nil, internalErr(err)
	}
	// Count from the graph THIS reconcile produced, not a fresh snapshot() that a
	// concurrent watcher tick could have swapped underneath us, so the reported counts
	// always describe this call's result. Fall back to the snapshot on a no-op pass
	// (rec.Graph is nil when nothing changed) and on a throttled replay, where the
	// remembered graph pointer may predate a watcher swap.
	g := rec.Graph
	if g == nil || throttled {
		g, _ = s.snapshot()
	}
	out := map[string]any{
		"reindexed": rec.Reindexed,
		"ms":        rec.Dur.Milliseconds(),
	}
	if throttled {
		out["throttled"] = true
		out["note"] = "a reindex ran moments ago; this is that pass's result. Retry in a few seconds to force a fresh one."
	}
	// A scope-confined caller must not learn out-of-scope volume: the global graph
	// totals AND the reconcile deltas (added/changed/removed, which span every scope)
	// leak it. Report only the caller's readable view; an unrestricted caller (nil
	// filter or nil AllowedRead, e.g. an admin or a solo run) gets the full numbers.
	if sf := scopeFromCtx(ctx); sf != nil && sf.AllowedRead != nil {
		out["nodes"], out["edges"] = scopedGraphCounts(g, sf)
	} else {
		out["added"] = rec.Added
		out["changed"] = rec.Changed
		out["removed"] = rec.Removed
		out["nodes"] = g.NodeCount()
		out["edges"] = g.EdgeCount()
	}
	// Refresh the source-code index too, so one mesh_reindex catches both note and
	// code edits. ok=false means code indexing is not enabled for this vault. The
	// counts are only returned to callers who may read the (dev-scoped) code index;
	// a scope-confined caller must not learn the code corpus volume (still refreshed,
	// just not reported), mirroring the code_search/neighbors gate. Skipped entirely on
	// a throttled replay: re-walking the code roots is the other half of the cost the
	// cooldown exists to bound.
	if !throttled {
		if cs, ok, cerr := s.reindexCode(); ok && cerr == nil && !codeScopeDenied(ctx) {
			out["code_files"] = cs.Files
			out["code_symbols"] = cs.Symbols
			out["code_edges"] = cs.Edges
		}
	}
	return textResult(out), nil
}

// toolHealth runs the lifecycle health pass (dead refs + overdue reviews) and
// returns the findings grouped by issue plus the current counts (incl. any
// contradiction rows the curator wrote).
func (s *Server) toolHealth(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		Issue string `json:"issue"`
	}
	_ = json.Unmarshal(raw, &a)
	// Role write gate: the health pass reads EVERY note file in the vault and persists
	// the findings + contradiction rows, so like mesh_reindex it is a state-mutating
	// expensive operation wearing a read tool's clothes. A read-only hosted viewer must
	// not be able to drive it repeatedly.
	if can, set := writeAllowed(ctx); set && !can {
		return nil, &rpcError{Code: codeInvalidParams, Message: "forbidden: your role is read-only"}
	}
	now := time.Now()
	if _, err := s.store.ComputeHealth(s.vaultRoot, now); err != nil {
		return nil, internalErr(err)
	}
	if _, err := s.store.ComputeContradictions(now); err != nil {
		return nil, internalErr(err)
	}
	findings, err := s.store.ListHealth(strings.TrimSpace(a.Issue))
	if err != nil {
		return nil, internalErr(err)
	}
	counts, _ := s.store.HealthCounts()
	if counts == nil {
		counts = map[string]int{}
	}
	// Unparseable notes never made it into the DB, which is exactly the bug: a note
	// with broken frontmatter vanishes from search and the graph with no signal.
	// Surface the ones the last reindex dropped so an operator can find and fix the
	// note that disappeared. They carry no note id or scope (they never parsed), so
	// they are operational findings, always shown, never scope-filtered out.
	//
	// DroppedNotes carries two distinct causes with two different remedies, so they get
	// two issue kinds rather than one misleading label: "unparseable" means fix the
	// frontmatter, "duplicate-id" means the note was quarantined because another note
	// already holds its id and one of the two needs a new one.
	for _, d := range s.store.DroppedNotes() {
		detail := ""
		if d.Err != nil {
			// The THIRD copy of the same shape, and the one that survived the sweep that
			// closed the other two: a relativized Path sitting next to a raw error, which
			// reads as already handled. These errors come from ParseFile, which os.ReadFile
			// calls with the ABSOLUTE path, so an unreadable note answered
			// detail = "open <abs vault root>/decisions/x.md: permission denied" while the
			// path field beside it said "decisions/x.md". Duplicate-id details name only
			// vault-relative paths, so the scrub leaves those untouched.
			detail = ScrubPathsUnder(d.Err.Error(), s.vaultRoot)
		}
		kind := "unparseable"
		if errors.Is(d.Err, index.ErrDuplicateNoteID) {
			kind = "duplicate-id"
		}
		if a.Issue != "" && a.Issue != kind {
			continue
		}
		findings = append(findings, index.HealthFinding{Issue: kind, Path: d.Path, Detail: detail})
		counts[kind]++
	}
	// Scope read check: findings carry a note id + path, so an unfiltered health pass
	// leaks the existence and paths of notes outside the caller's scope (and the global
	// counts do the same in aggregate). Drop out-of-scope findings and recompute counts
	// from what is left. A nil filter (solo / no-scope hub) leaves both untouched.
	if sf := scopeFromCtx(ctx); sf != nil {
		kept := findings[:0]
		scoped := make(map[string]int, len(counts))
		for _, f := range findings {
			if f.Issue == "unparseable" {
				// No parsed note, so no scope to check: an operational finding shown to all.
				kept = append(kept, f)
				scoped[f.Issue]++
				continue
			}
			sc, serr := s.store.NoteScope(f.NoteID)
			if serr != nil || !sf.allowsRead(sc) {
				continue
			}
			kept = append(kept, f)
			scoped[f.Issue]++
		}
		findings = kept
		counts = scoped
	}
	return textResult(map[string]any{"findings": findings, "counts": counts}), nil
}

// toolSetupHooks drives the session-hook onboarding: status returns the pitch +
// questions for the agent to run the conversation; install/uninstall apply it. The
// hooks make the agent read the mesh at session start and write back at the end.
func (s *Server) toolSetupHooks(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	// This tool writes .claude/settings.json under a caller-supplied project_dir on the
	// SERVER host. It only makes sense for the local binary configuring its own agent;
	// over any remote transport it lets an authenticated caller drive a server-side
	// filesystem write at a path they choose. Gate POSITIVELY on the local stdio marker:
	// the previous check ("is the hosted write capability set?") failed OPEN on
	// `mesh mcp --http`, which sets neither marker, so a bearer-token holder reached it.
	if !localOperator(ctx) {
		return nil, &rpcError{Code: codeInvalidParams, Message: "mesh_setup_hooks is only available on a local mesh install over stdio, not over a network transport"}
	}
	var a struct {
		Action     string `json:"action"`
		ProjectDir string `json:"project_dir"`
		ReadOnly   bool   `json:"read_only"`
		DryRun     bool   `json:"dry_run"`
	}
	json.Unmarshal(raw, &a)
	proj := a.ProjectDir
	if proj == "" {
		proj, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(proj); err == nil {
		proj = abs
	}
	bin, _ := os.Executable()
	if bin == "" {
		bin = "mesh"
	}
	vaultAbs, _ := filepath.Abs(s.vaultRoot)

	switch a.Action {
	case "install":
		// project_dir must already BE a project: hooks.Install does MkdirAll(.claude)
		// under it, so accepting a path that does not exist turns a typo (or a
		// hallucinated path from the agent) into directory creation anywhere this
		// process can write. Installing hooks into a directory nobody is working in
		// is never the intent, so requiring it to exist costs nothing.
		if fi, serr := os.Stat(proj); serr != nil || !fi.IsDir() {
			return nil, &rpcError{Code: codeInvalidParams, Message: "project_dir must be an existing directory"}
		}
		res, err := hooks.Install(hooks.Options{ProjectDir: proj, Vault: vaultAbs, Bin: bin, EnforceWriteback: !a.ReadOnly, DryRun: a.DryRun})
		if err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
		}
		out := map[string]any{"settings_path": res.SettingsPath, "added": res.Added}
		if a.DryRun {
			out["dry_run"] = true
			out["preview"] = res.Preview
		} else if len(res.Added) == 0 {
			out["note"] = "already installed; nothing to do"
		} else {
			out["installed"] = true
			out["next"] = "Tell the user to run /hooks in Claude Code to verify, then restart the session for it to take effect."
		}
		return textResult(out), nil
	case "uninstall":
		n, p, err := hooks.Uninstall(proj)
		if err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
		}
		return textResult(map[string]any{"removed": n, "settings_path": p}), nil
	default:
		st, _ := hooks.GetStatus(proj)
		return textResult(map[string]any{
			"status":       st,
			"vault":        vaultAbs,
			"project_dir":  proj,
			"what_it_does": "Wires two Claude Code session hooks: SessionStart runs `mesh orient` so you begin every session having read the mesh (its entry points + recent changes + how to retrieve); Stop nudges you once to write back what you learned (mesh_append_note) before finishing.",
			"why":          "It turns Mesh from a tool you must remember to use into the default: every session starts informed and ends a little smarter, so knowledge compounds across sessions and teammates instead of being relearned. This is the flywheel, the real superpower.",
			"clarify":      "These are Claude Code SESSION hooks, not git pre/post-push hooks (those are a separate layer for code pushes).",
			"questions_to_ask_the_user": []string{
				"Set up the session hooks for this project now? (recommended)",
				"Enforce write-back too (a one-time Stop nudge per session), or read-only (just the start-of-session orientation)?",
				"Is " + proj + " the right project directory? Its .claude/settings.json will be edited.",
			},
			"how_to_apply": "After they answer: call mesh_setup_hooks action=install (read_only=true for orientation-only). Pass dry_run=true first if they want to see the exact settings.json. Then they restart the session and verify with /hooks.",
		}), nil
	}
}

// searchLimitDefault/Max and searchBudgetDefault bound one mesh_search. Without them
// a single call returned every FTS-matching note as a card (Retrieve only FLOORS the
// limit, and packToBudget is skipped when Budget is 0), which blows out the calling
// agent's context and, on the hub, does corpus-sized work per request. The ceilings
// match what graph_tools.go already enforces on the other tools.
const (
	searchLimitDefault  = 20
	searchLimitMax      = 100
	searchBudgetDefault = 8000
)

// SearchLimitDefault/Max and SearchBudgetDefault export the caps above for the OTHER
// surface over the same retriever: internal/web's GET /api/search, which shipped with
// neither and let ?limit=100000 return 401 cards / ~31k tokens / 114 KB from a 400-note
// vault, and made the hub do corpus-sized work per unauthenticated-ish page request.
// Exported as aliases rather than as a rename so package-internal callers stay as they
// were, and so the two surfaces cannot drift apart again by editing one number.
const (
	SearchLimitDefault  = searchLimitDefault
	SearchLimitMax      = searchLimitMax
	SearchBudgetDefault = searchBudgetDefault
)

// searchCard is the MCP wire shape of a retrieval card: the retriever's card plus the
// provenance the agent needs to judge the snippet. retrieve.Card carries no source
// field, so Source is derived here from the note path (see importedSource) and set
// only for third-party ingested notes, keeping the common card byte-identical.
type searchCard struct {
	retrieve.Card
	Source string `json:"Source,omitempty"`
}

func (s *Server) toolSearch(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		Query  string `json:"query"`
		Budget int    `json:"budget"`
		Limit  int    `json:"limit"`
	}
	json.Unmarshal(raw, &a)
	if strings.TrimSpace(a.Query) == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "query is required"}
	}
	limit := clampLimit(a.Limit, searchLimitDefault, searchLimitMax)
	budget := a.Budget
	if budget <= 0 {
		budget = searchBudgetDefault
	}
	_, retriever := s.snapshot()
	var allowed map[string]bool
	if sf := scopeFromCtx(ctx); sf != nil {
		allowed = sf.AllowedRead // nil-safe: nil => retriever does not filter
	}
	// Retrieve UNPACKED and pack below instead. labelCard rewrites the snippet of every
	// connector-ingested card AFTER retrieval, so packing inside Retrieve prices bytes
	// this tool does not send: measured on a 300-note imported vault, the default budget
	// of 8000 shipped 11284 tokens (+41%) while reporting 7988, and 1000/2000/3000 were
	// each ~40% over. The budget exists so an agent can bound what one call costs its
	// context, so it has to be measured on the FINAL form. Budget 0 here is not the
	// /api/search defect (internal/web/search_cap_test.go): limit is already clamped
	// above, so the unpacked set is bounded, and searchCardTokens packs it below.
	cards, err := retriever.Retrieve(ctx, a.Query, retrieve.Options{Limit: limit, AllowedScopes: allowed})
	if err != nil {
		return nil, internalErr(err)
	}
	cards = retrieve.PackToBudget(cards, budget, searchCardTokens)
	_ = s.store.IncrMetric("queries", 1) // ROI telemetry (best-effort)
	return textResult(map[string]any{
		"cards": labelCards(cards),
		// Reported with the SAME cost function the packer used, so the number the agent
		// reads is the number that was packed to.
		"tokens": retrieve.TotalTokensFunc(cards, searchCardTokens),
	}), nil
}

// labelCard marks a card whose note came from a connector import: it stamps the source
// and wraps the snippet in the data envelope, so third-party text can never reach the
// agent as an unlabelled instruction-shaped span (the instruction boundary the HTML and
// TTY sinks already have). A card from the team's own notes passes through untouched.
func labelCard(c retrieve.Card) searchCard {
	sc := searchCard{Card: c}
	if src, ok := importedSource(c.Path); ok {
		sc.Source = src
		sc.Snippet = wrapUntrusted(src, "", c.Snippet)
	}
	return sc
}

func labelCards(cards []retrieve.Card) []searchCard {
	out := make([]searchCard, 0, len(cards))
	for _, c := range cards {
		out = append(out, labelCard(c))
	}
	return out
}

// searchCardTokens prices a card as mesh_search actually sends it: the LABELLED card,
// envelope and Source field included. This is the retrieve.CardCost the packer runs on,
// which is what keeps the bytes sent equal to the bytes budgeted. It must stay in sync
// with labelCard by construction, so it calls it rather than re-deriving the shape.
func searchCardTokens(c retrieve.Card) int {
	sc := labelCard(c)
	b, err := json.Marshal(sc)
	if err != nil {
		// A card that cannot be marshaled cannot be sent either. Price the wrapped card
		// with the retriever's own counter so the packer keeps making progress instead
		// of treating it as free.
		return retrieve.TotalTokens([]retrieve.Card{sc.Card}) + retrieve.EstimateTokens(sc.Source)
	}
	return retrieve.EstimateTokens(string(b))
}

func (s *Server) toolFetch(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		ID     string `json:"id"`
		Anchor string `json:"anchor"`
	}
	json.Unmarshal(raw, &a)
	rel, err := s.store.NotePath(a.ID)
	if err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown note id", Data: a.ID}
	}
	// Scope read check: a direct fetch resolves id -> path -> file, bypassing the
	// retriever's filter, so gate it here. Return the SAME opaque "unknown note id" a
	// missing note returns, so a scoped caller can't probe which ids exist.
	if sf := scopeFromCtx(ctx); sf != nil {
		sc, serr := s.store.NoteScope(a.ID)
		if serr != nil || !sf.allowsRead(sc) {
			return nil, &rpcError{Code: codeInvalidParams, Message: "unknown note id", Data: a.ID}
		}
	}
	data, err := os.ReadFile(filepath.Join(s.vaultRoot, rel))
	if err != nil {
		return nil, internalErr(err)
	}
	body := string(data)
	// Provenance is read from the WHOLE file, before any anchor slicing: an anchored
	// fetch cuts the frontmatter off, and that is exactly the case where the agent
	// would otherwise get a bare span of third-party prose with nothing saying so.
	src, srcURL := frontmatterProvenance(body)
	if a.Anchor != "" {
		sec, ok := sectionByAnchor(body, a.Anchor)
		if !ok {
			return nil, &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf(
				"note %q has no section with anchor %q; available anchors: %s",
				a.ID, a.Anchor, strings.Join(anchorsOf(body), ", "))}
		}
		body = sec
	}
	// Connector-ingested text is data, not instructions. Wrap it in the envelope the
	// contract describes so the agent has an explicit boundary. The frontmatter source
	// is authoritative (ingest stamps source: import:<connector>); the path check is
	// the fallback for a note whose frontmatter was hand-edited away.
	if !strings.HasPrefix(src, importSourcePrefix) {
		if ps, ok := importedSource(rel); ok {
			src = ps
		}
	}
	if strings.HasPrefix(src, importSourcePrefix) {
		body = wrapUntrusted(src, srcURL, body)
	}
	_ = s.store.IncrMetric("fetches", 1)            // ROI telemetry (best-effort)
	_ = s.store.IncrMetric("fetch:"+a.ID, 1)        // per-note reuse (most-reused list)
	_ = s.store.RecordReuse(a.ID, flywheelReuseGap) // flywheel: a later fetch = the next run inheriting it
	return rawText(body), nil
}

func (s *Server) toolGodNodes(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		Limit int `json:"limit"`
	}
	json.Unmarshal(raw, &a)
	// Cap it like every other limit-taking tool. This was the one that called neither
	// clampLimit nor anything else, so mesh_god_nodes{"limit":1000000} returned the whole
	// note corpus as "hubs" - the orientation call, the one an agent makes FIRST, was the
	// one that could blow out its context before it had read anything.
	a.Limit = clampLimit(a.Limit, 10, 100)
	type hub struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		Path      string `json:"path"`
		Degree    int    `json:"degree"`
		Community int    `json:"community"`
	}
	g, _ := s.snapshot()
	sf := scopeFromCtx(ctx)
	var hubs []hub
	for _, n := range g.Nodes() {
		if n.Kind != "note" {
			continue
		}
		if !sf.allowsNode(n) { // hide hubs the caller cannot read (no title enumeration)
			continue
		}
		hubs = append(hubs, hub{n.NoteID, n.Label, n.NotePath, n.Degree, n.Community})
	}
	sort.Slice(hubs, func(i, j int) bool {
		if hubs[i].Degree != hubs[j].Degree {
			return hubs[i].Degree > hubs[j].Degree
		}
		return hubs[i].ID < hubs[j].ID
	})
	if len(hubs) > a.Limit {
		hubs = hubs[:a.Limit]
	}
	return textResult(map[string]any{"hubs": hubs}), nil
}

// changedSince* bound one delta. ChangedSince is a bare "WHERE mtime > ? ORDER BY mtime
// DESC" with no LIMIT and no validation, and the same parameter had two opposite traps.
//
// Too small: since=0 (or a negative) returned every note in the vault, unbounded, from
// the tool an agent calls on RESUME, when its context is already loaded.
//
// Too large: since is UNIX SECONDS, and passing milliseconds is the single most likely
// caller mistake, because most runtimes hand out milliseconds by default. A millisecond
// stamp is a year-57578 timestamp, so the query matched nothing and the tool answered
// "changed": null. The agent reads that as "nothing changed since I left" and proceeds
// on stale context, which is worse than an error: it is a wrong answer with no signal.
// So a future `since` is refused, and the refusal names the likely cause.
const (
	changedSinceLimitDefault = 100
	changedSinceLimitMax     = 500
	// changedSinceFutureSlack is how far ahead of the server's clock a `since` may sit
	// before it is a caller mistake rather than clock skew between two machines.
	changedSinceFutureSlack = int64(3600) // seconds
)

func (s *Server) toolChangedSince(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		Since int64 `json:"since"`
		Limit int   `json:"limit"`
	}
	json.Unmarshal(raw, &a)
	now := time.Now().Unix()
	if a.Since > now+changedSinceFutureSlack {
		msg := fmt.Sprintf("since=%d is in the future (server time is %d), so this can only ever report that nothing changed", a.Since, now)
		if a.Since/1000 <= now+changedSinceFutureSlack {
			msg += fmt.Sprintf("; that looks like MILLISECONDS, and since is UNIX SECONDS: pass %d", a.Since/1000)
		}
		return nil, &rpcError{Code: codeInvalidParams, Message: msg}
	}
	if a.Since < 0 {
		a.Since = 0 // "everything" is a legitimate ask; the limit below is what bounds it
	}
	limit := clampLimit(a.Limit, changedSinceLimitDefault, changedSinceLimitMax)
	refs, err := s.store.ChangedSince(a.Since)
	if err != nil {
		return nil, internalErr(err)
	}
	// Scope read check: each ref exposes a note id, path and mtime, so an unfiltered
	// delta lets a scoped caller enumerate notes outside their scope. Drop the ones
	// they cannot read. A nil filter (solo / no-scope hub) leaves the list untouched.
	if sf := scopeFromCtx(ctx); sf != nil {
		kept := refs[:0]
		for _, r := range refs {
			sc, serr := s.store.NoteScope(r.ID)
			if serr != nil || !sf.allowsRead(sc) {
				continue
			}
			kept = append(kept, r)
		}
		refs = kept
	}
	// An explicit truncated flag is what lets a caller tell a CAPPED delta from a
	// complete one. Without it a capped list is indistinguishable from "that is all
	// there is", which is the same silent-wrong-answer shape as the millisecond case.
	// The refs are newest-first, so the cap keeps the newest and drops the oldest.
	total := len(refs)
	truncated := total > limit
	if truncated {
		refs = refs[:limit]
	}
	if refs == nil {
		refs = []index.NoteRef{} // never "changed": null; an empty delta says so as [] plus count 0
	}
	out := map[string]any{"changed": refs, "count": len(refs)}
	if truncated {
		out["truncated"] = true
		out["total"] = total
		out["limit"] = limit
		out["note"] = fmt.Sprintf("%d notes changed since that timestamp; only the %d most recent are listed. "+
			"Pass a more recent `since`, or a larger `limit` (max %d).", total, limit, changedSinceLimitMax)
	}
	return textResult(out), nil
}

func (s *Server) toolWrite(ctx context.Context, raw json.RawMessage, forceType string) (any, *rpcError) {
	var a struct {
		Type       string   `json:"type"`
		Title      string   `json:"title"`
		Do         string   `json:"do"`
		Dont       string   `json:"dont"`
		Why        string   `json:"why"`
		Related    []string `json:"related"`
		Tags       []string `json:"tags"`
		Status     string   `json:"status"`
		Severity   string   `json:"severity"`
		Author     string   `json:"author"`
		Source     string   `json:"source"`
		SourceURL  string   `json:"source_url"`
		Confidence string   `json:"confidence"`
		ReviewBy   string   `json:"review_by"`
		Scope      string   `json:"scope"`
	}
	json.Unmarshal(raw, &a)
	// Role write gate (independent of scope): a read-only hosted client must not
	// create notes. This is enforced FIRST because the scope gate below leaves
	// sf nil when the team has not configured scoping, which would otherwise skip
	// the only write check and let a viewer-role token write. Unset (local solo
	// binary) means the caller owns the vault, so writes are allowed there.
	if can, set := writeAllowed(ctx); set && !can {
		return nil, &rpcError{Code: codeInvalidParams, Message: "forbidden: your role is read-only"}
	}
	// Scope write gate: a scoped caller may only create notes in a scope they can
	// write, and the new note is stamped with that scope. A nil filter (solo / no-scope
	// hub) leaves noteScope empty so the note carries no scope frontmatter (= dev).
	var noteScope []string
	if sf := scopeFromCtx(ctx); sf != nil {
		want := strings.TrimSpace(a.Scope)
		if want == "" {
			want = sf.WriteScope
		}
		if want == "" {
			return nil, &rpcError{Code: codeInvalidParams, Message: "your account is not in a single scope; pass an explicit `scope`"}
		}
		if sf.CanWrite == nil || !sf.CanWrite(want) {
			return nil, &rpcError{Code: codeInvalidParams, Message: "forbidden: you cannot write notes in scope " + want}
		}
		noteScope = []string{want}
	}
	t := a.Type
	if forceType != "" {
		t = forceType
	}
	if t == "" {
		t = "note"
	}
	// Provenance: default source to "agent" and stamp the calling tool so the audit
	// trail, lifecycle checks, and contributor view know who wrote this.
	s.mu.RLock()
	agent := s.agent
	s.mu.RUnlock()
	source := strings.TrimSpace(a.Source)
	if source == "" {
		source = "agent"
	}
	// `related` is optional, and an agent that omits it writes a note with no
	// note-to-note edge: findable by full-text, invisible to graph proximity, and a
	// disconnected dot in the web app. That is not a rare slip. On 2026-08-06 the
	// reference vault carried 111 such notes out of 1140, and they skewed tier-0
	// (67 gotchas, 31 decisions, 13 post-mortems), so the material retrieval is meant
	// to surface first was the least reachable. Derive the links from the note's own
	// text when the caller supplies none; an explicit list always wins, including an
	// explicit decision to pass none that survives as an empty non-nil slice.
	related := a.Related
	if len(related) == 0 {
		g, rt := s.snapshot()
		related = relate.Derive(ctx, rt, g,
			strings.TrimSpace(a.Title+"\n"+a.Do+"\n"+a.Why), "", a.Tags, 3)
	}
	res, err := vault.CreateNote(s.vaultRoot, vault.NewNoteSpec{
		Type: vault.NoteType(t), Title: a.Title, Do: a.Do, Dont: a.Dont, Why: a.Why,
		Related: related, Tags: a.Tags, Status: a.Status, Severity: a.Severity,
		Author: a.Author, Agent: agent, Source: source, SourceURL: a.SourceURL,
		Confidence: a.Confidence, ReviewBy: a.ReviewBy, By: agent, Scope: noteScope,
	})
	if err != nil {
		// Do NOT echo a raw error here. Everything vault.CreateNote raises about the
		// FILESYSTEM names the note's absolute path, so a too-long title came back as
		// "open /srv/hub/vault/gotchas/<slug>.md: file name too long" and handed the
		// agent the server's absolute vault root - the exact leak the success path 30
		// lines below goes out of its way to prevent by relativizing res.Path. Errors
		// about the caller's own input (vault.ErrInvalidSpec, which now covers the
		// too-long title) are authored in that package, carry no path, and are the ones
		// worth reading, so those go back verbatim.
		if errors.Is(err, vault.ErrInvalidSpec) {
			return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
		}
		slog.Error("mesh write: the note could not be created", "type", t, "error", err)
		return nil, &rpcError{Code: codeInvalidParams, Message: "the note could not be written: " + ScrubPathsUnder(err.Error(), s.vaultRoot)}
	}
	// Make the new note queryable through the INCREMENTAL path, not a full reindex.
	// reload() re-walked and re-parsed the entire vault and rewrote every notes /
	// search_index / nodes / edges row in one transaction to publish one small file, so
	// write-back cost scaled with vault size instead of with the change; because rebuilds
	// serialize on reloadMu, concurrent hosted write-backs then queued head to tail
	// against the hub's 30s write timeout. reconcileOnce sees exactly this file as one
	// Added path (authoritative, so a same-second mtime cannot hide it) and rebuilds the
	// graph from the parsed-note cache the startup load seeded.
	//
	// A failure here must NOT be reported as a failed write. The note is already
	// durably on disk, so returning -32603 tells the agent "nothing was written",
	// it retries, and Mesh mints a near-duplicate with a -2 id suffix. Two notes on
	// one topic split retrieval and the next agent reads whichever ranks first,
	// which is worse than the error. That happened three times (2026-07-26, 07-28,
	// 07-30), the last two with a warning gotcha already in the vault, which is
	// what settled that a note cannot fix a tool that misreports its own outcome.
	//
	// reconcileOnce writes to the store, so it fails whenever a `mesh sync --watch`
	// daemon holds the write lock. That is a refresh problem, never a durability
	// problem. So: succeed, and say the index is stale, so the caller knows to run
	// mesh_reindex and knows not to retry the write.
	var indexStale string
	if _, err := s.reconcileOnce(true); err != nil {
		slog.Error("mesh write: the note was saved but the index did not refresh",
			"id", res.ID, "path", res.Path, "error", err)
		// Scrubbed like every other message that leaves this process. This one is easy to
		// miss because it sits right next to a "path" field that IS relativized, but the
		// reconcile walks the vault, so its errors are filesystem errors: an unreadable
		// subdirectory came back as "open <abs vault root>/locked: permission denied" and
		// shipped the whole server layout inside the staleness warning.
		indexStale = ScrubPathsUnder(err.Error(), s.vaultRoot)
	}
	_ = s.store.IncrMetric("writes", 1)         // ROI telemetry (best-effort)
	_ = s.store.RecordWriteback(res.ID, source) // flywheel: stamp authoring time for reuse measurement
	// Return a vault-relative path, never the server's absolute filesystem path:
	// on a hosted hub the absolute path would leak the server's absolute vault path
	// to the agent.
	notePath := res.Path
	if rel, err := filepath.Rel(s.vaultRoot, res.Path); err == nil && !strings.HasPrefix(rel, "..") {
		notePath = rel
	} else {
		notePath = filepath.Base(res.Path)
	}
	out := map[string]any{"id": res.ID, "path": notePath, "when": res.When, "todo": res.TODOs}
	if indexStale != "" {
		out["index_stale"] = true
		out["index_error"] = indexStale
		out["warning"] = "The note IS saved at the path above. Only the index refresh failed, " +
			"so it is not queryable yet. Do NOT call this tool again: a retry creates a " +
			"duplicate note. Call mesh_reindex instead."
	}
	return textResult(out), nil
}

// ScrubPathsUnder makes an error message safe to hand a caller, for a process serving the
// vault at root. It is THE entry point for every surface: internal/web's promote handler
// calls it too, because the identical leak shipped on both surfaces and a second private
// copy of the logic is exactly how one of them keeps the bug after the other is fixed.
//
// The symlink-resolved spelling of root is scrubbed as well, because the server can be
// started with one spelling while the kernel names the other in an error (a symlinked
// /tmp resolves to /private/tmp on macOS).
func ScrubPathsUnder(msg, root string) string {
	roots := []string{root}
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != root {
		roots = append(roots, resolved)
	}
	return scrubPaths(msg, roots...)
}

// scrubPaths rewrites every absolute path in an error message down to its base name, so
// a message that reaches a remote agent cannot carry the server's directory layout.
// It replaces rather than deletes, because the base name (the note's own slug) is the
// part the caller supplied and the part that makes the message useful. Full detail is
// logged server-side by the caller.
//
// roots are the absolute paths this process KNOWS it serves. They are matched as
// substrings, which is the only thing that works for a root CONTAINING A SPACE: this
// estate's own vault lives under ".../Automation HQ/...", and the whitespace scan below
// saw that as two tokens, rewrote the first to "<path>/Automation" and left the tail
// ("HQ/vault/gotchas/x.md") standing, so most of the layout still shipped.
//
// The whitespace scan stays as the fallback for prefixes this process was never told
// about, which is what a root-only comparison would miss.
func scrubPaths(msg string, roots ...string) string {
	known := append([]string(nil), roots...)
	// Longest first, so a nested root is not half-consumed by its parent.
	sort.Slice(known, func(i, j int) bool { return len(known[i]) > len(known[j]) })
	for _, root := range known {
		msg = scrubRoot(msg, root)
	}
	fields := strings.Fields(msg)
	for i, f := range fields {
		trimmed := strings.TrimRight(f, ":;,")
		if !filepath.IsAbs(trimmed) {
			continue
		}
		fields[i] = "<path>/" + filepath.Base(trimmed) + f[len(trimmed):]
	}
	return strings.Join(fields, " ")
}

// scrubRoot cuts every path that STARTS at a known root down to "<path>/<base>". It scans
// from the END of the root, so whitespace inside the root cannot end the match: that is
// the whole reason it exists, and why tokenizing on whitespace alone was not enough.
func scrubRoot(msg, root string) string {
	root = strings.TrimRight(root, string(filepath.Separator))
	// A relative root would match all over a message, and "/" trims to "" here, so both
	// are refused rather than allowed to swallow the text.
	if root == "" || !filepath.IsAbs(root) {
		return msg
	}
	var b strings.Builder
	for {
		i := strings.Index(msg, root)
		if i < 0 {
			break
		}
		end := i + len(root)
		for end < len(msg) && !endsPath(msg[end]) {
			end++
		}
		span := msg[i:end]
		tail := strings.TrimRight(span, ":;,")                      // trailing punctuation is prose, not path
		path := strings.TrimRight(tail, string(filepath.Separator)) // "<root>/" still names the root
		b.WriteString(msg[:i])
		b.WriteString("<path>")
		if base := filepath.Base(path); path != root && base != "." && base != string(filepath.Separator) {
			b.WriteString(string(filepath.Separator))
			b.WriteString(base)
		}
		b.WriteString(span[len(tail):]) // put the punctuation back
		msg = msg[end:]
	}
	b.WriteString(msg)
	return b.String()
}

// endsPath reports whether c cannot be part of a filesystem path inside an error message.
// Whitespace and quotes end one; ':' does not, because it is both the separator in
// "open <path>: reason" and legal in a filename, so the trailing run is trimmed instead.
func endsPath(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '"', '\'', '`':
		return true
	}
	return false
}

// flywheelReuseGap is how long after a write-back a fetch must land to count as reuse
// by a LATER session rather than a re-read inside the same work burst (the
// cross-session proxy that works for both the solo CLI and the long-lived hub).
const flywheelReuseGap = 600 // seconds (10 min)

// sectionByAnchor returns the markdown of the heading section whose slug matches
// anchor (from that heading until the next heading of the same or higher level), and
// whether such a heading existed. A miss must NOT fall back to the whole note: this is a
// narrowing function, and returning its unnarrowed input turned one wrong character in an
// anchor into a multi-megabyte reply that no caller asked for and none can afford.
func sectionByAnchor(body, anchor string) (string, bool) {
	lines := strings.Split(body, "\n")
	start, level := -1, 0
	inFence := false
	for i, ln := range lines {
		if isCodeFence(ln) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue // a '#' line inside a code block is not a heading
		}
		h := strings.TrimLeft(ln, "#")
		lvl := len(ln) - len(h)
		if lvl >= 1 && lvl <= 6 && strings.HasPrefix(h, " ") {
			if anchorMatches(strings.TrimSpace(h), anchor) {
				start, level = i, lvl
				break
			}
		}
	}
	if start < 0 {
		return "", false
	}
	end := len(lines)
	inFence = false
	for i := start + 1; i < len(lines); i++ {
		ln := lines[i]
		if isCodeFence(ln) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue // do not let a '#' inside a fenced block end the section early
		}
		h := strings.TrimLeft(ln, "#")
		lvl := len(ln) - len(h)
		if lvl >= 1 && lvl <= level && strings.HasPrefix(h, " ") {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n"), true
}

// anchorsOf lists the slugs a note actually offers, so an anchor miss can name the real
// options instead of leaving the caller to guess. It is the same vault.Slugify the index
// uses to build heading nodes, so what it lists is exactly what resolves.
func anchorsOf(body string) []string {
	var out []string
	inFence := false
	for _, ln := range strings.Split(body, "\n") {
		if isCodeFence(ln) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		h := strings.TrimLeft(ln, "#")
		if lvl := len(ln) - len(h); lvl >= 1 && lvl <= 6 && strings.HasPrefix(h, " ") {
			if s := vault.Slugify(strings.TrimSpace(h)); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// isCodeFence reports whether a line opens or closes a fenced code block (``` / ~~~).
func isCodeFence(ln string) bool {
	t := strings.TrimLeft(ln, " \t")
	return strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")
}

// anchorMatches reports whether a heading answers to the given anchor. It accepts the
// CURRENT slug (vault.Slugify, which folds diacritics) and the legacy ASCII-only one,
// because for a while both were minted for the same heading and the old ones are still
// in circulation: the index stores a heading node per anchor and only refreshes it on
// reindex, so between a binary upgrade and the next reindex a caller can be holding
// "tg-rder" for a heading that now slugs to "atgarder". Accepting both costs one string
// compare on the miss path and keeps those fetches working.
func anchorMatches(heading, anchor string) bool {
	if anchor == "" {
		return false
	}
	return vault.Slugify(heading) == anchor || slugifyLegacy(heading) == anchor
}

// slugifyLegacy reproduces the slug Mesh emitted before vault.Slugify learned to fold
// diacritics: keep [a-z0-9], collapse everything else to a dash. It is a LOOKUP
// fallback only (see anchorMatches). Nothing new is ever minted in this form.
func slugifyLegacy(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
