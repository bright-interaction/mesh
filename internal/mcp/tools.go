// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/hooks"
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
			"description": "Fused retrieval over the vault (full-text + graph proximity), tier-0 boosted (decisions/gotchas/post-mortems first). Returns ranked cards. Pass a token budget to get the best bundle that fits. Start here.",
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
			"description": "Notes modified after a unix timestamp, newest first. Pull only deltas when resuming.",
			"inputSchema": obj(map[string]any{"type": "object", "required": []string{"since"}, "properties": map[string]any{"since": intp}}),
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
			"description": "Locate SOURCE-CODE symbols (functions, types, methods, classes) by name across the indexed repos, ranked by name match. Returns cards with a file:line locator and signature so you jump straight to a definition instead of grepping the tree. Use this for 'where is X defined / what's in this area of the code'. This is the code index; mesh_search is for notes/knowledge.",
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
	case "mesh_setup_hooks":
		return s.toolSetupHooks(p.Arguments)
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "unknown tool", Data: p.Name}
	}
}

// toolReindex forces an authoritative reconcile so an agent editing note files
// directly (via the editor or CLI) can make its edits queryable on demand, instead
// of waiting on the --watch debounce or restarting a no-watch server. Authoritative
// = full content-hash check, so it also catches an edit that did not move the mtime.
func (s *Server) toolReindex(ctx context.Context) (any, *rpcError) {
	rec, err := s.reconcileOnce(true)
	if err != nil {
		return nil, internalErr(err)
	}
	// Count from the graph THIS reconcile produced, not a fresh snapshot() that a
	// concurrent watcher tick could have swapped underneath us, so the reported counts
	// always describe this call's result. Fall back to the snapshot on a no-op pass
	// (rec.Graph is nil when nothing changed).
	g := rec.Graph
	if g == nil {
		g, _ = s.snapshot()
	}
	out := map[string]any{
		"reindexed": rec.Reindexed,
		"ms":        rec.Dur.Milliseconds(),
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
	// just not reported), mirroring the code_search/neighbors gate.
	if cs, ok, cerr := s.reindexCode(); ok && cerr == nil && !codeScopeDenied(ctx) {
		out["code_files"] = cs.Files
		out["code_symbols"] = cs.Symbols
		out["code_edges"] = cs.Edges
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
	// Scope read check: findings carry a note id + path, so an unfiltered health pass
	// leaks the existence and paths of notes outside the caller's scope (and the global
	// counts do the same in aggregate). Drop out-of-scope findings and recompute counts
	// from what is left. A nil filter (solo / no-scope hub) leaves both untouched.
	if sf := scopeFromCtx(ctx); sf != nil {
		kept := findings[:0]
		scoped := make(map[string]int, len(counts))
		for _, f := range findings {
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
func (s *Server) toolSetupHooks(raw json.RawMessage) (any, *rpcError) {
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
	_, retriever := s.snapshot()
	var allowed map[string]bool
	if sf := scopeFromCtx(ctx); sf != nil {
		allowed = sf.AllowedRead // nil-safe: nil => retriever does not filter
	}
	cards, err := retriever.Retrieve(ctx, a.Query, retrieve.Options{Limit: a.Limit, Budget: a.Budget, AllowedScopes: allowed})
	if err != nil {
		return nil, internalErr(err)
	}
	_ = s.store.IncrMetric("queries", 1) // ROI telemetry (best-effort)
	return textResult(map[string]any{"cards": cards, "tokens": retrieve.TotalTokens(cards)}), nil
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
	if a.Anchor != "" {
		body = sectionByAnchor(body, a.Anchor)
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
	if a.Limit <= 0 {
		a.Limit = 10
	}
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

func (s *Server) toolChangedSince(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		Since int64 `json:"since"`
	}
	json.Unmarshal(raw, &a)
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
	return textResult(map[string]any{"changed": refs}), nil
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
	res, err := vault.CreateNote(s.vaultRoot, vault.NewNoteSpec{
		Type: vault.NoteType(t), Title: a.Title, Do: a.Do, Dont: a.Dont, Why: a.Why,
		Related: a.Related, Tags: a.Tags, Status: a.Status, Severity: a.Severity,
		Author: a.Author, Agent: agent, Source: source, SourceURL: a.SourceURL,
		Confidence: a.Confidence, ReviewBy: a.ReviewBy, By: agent, Scope: noteScope,
	})
	if err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
	}
	if err := s.reload(); err != nil {
		return nil, internalErr(err)
	}
	_ = s.store.IncrMetric("writes", 1)         // ROI telemetry (best-effort)
	_ = s.store.RecordWriteback(res.ID, source) // flywheel: stamp authoring time for reuse measurement
	// Return a vault-relative path, never the server's absolute filesystem path:
	// on the hosted hub the absolute path would leak /opt/mesh-hub/... to the agent.
	notePath := res.Path
	if rel, err := filepath.Rel(s.vaultRoot, res.Path); err == nil && !strings.HasPrefix(rel, "..") {
		notePath = rel
	} else {
		notePath = filepath.Base(res.Path)
	}
	return textResult(map[string]any{"id": res.ID, "path": notePath, "when": res.When, "todo": res.TODOs}), nil
}

// flywheelReuseGap is how long after a write-back a fetch must land to count as reuse
// by a LATER session rather than a re-read inside the same work burst (the
// cross-session proxy that works for both the solo CLI and the long-lived hub).
const flywheelReuseGap = 600 // seconds (10 min)

// sectionByAnchor returns the markdown of the heading section whose slug matches
// anchor (from that heading until the next heading of the same or higher level).
func sectionByAnchor(body, anchor string) string {
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
			if slugify(strings.TrimSpace(h)) == anchor {
				start, level = i, lvl
				break
			}
		}
	}
	if start < 0 {
		return body // anchor not found; hand back the whole note
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
	return strings.Join(lines[start:end], "\n")
}

// isCodeFence reports whether a line opens or closes a fenced code block (``` / ~~~).
func isCodeFence(ln string) bool {
	t := strings.TrimLeft(ln, " \t")
	return strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")
}

func slugify(s string) string {
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
