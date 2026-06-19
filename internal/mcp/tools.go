package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

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

func (s *Server) handleToolsCall(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "bad params"}
	}
	switch p.Name {
	case "mesh_search":
		return s.toolSearch(ctx, p.Arguments)
	case "mesh_fetch":
		return s.toolFetch(p.Arguments)
	case "mesh_god_nodes":
		return s.toolGodNodes(p.Arguments)
	case "mesh_changed_since":
		return s.toolChangedSince(p.Arguments)
	case "mesh_neighbors":
		return s.toolNeighbors(p.Arguments)
	case "mesh_community":
		return s.toolCommunity(p.Arguments)
	case "mesh_append_note":
		return s.toolWrite(p.Arguments, "")
	case "mesh_write_entity":
		return s.toolWrite(p.Arguments, "entity")
	case "mesh_reindex":
		return s.toolReindex()
	case "mesh_code_search":
		return s.toolCodeSearch(p.Arguments)
	case "mesh_code_neighbors":
		return s.toolCodeNeighbors(p.Arguments)
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
func (s *Server) toolReindex() (any, *rpcError) {
	rec, err := s.reconcileOnce(true)
	if err != nil {
		return nil, internalErr(err)
	}
	g, _ := s.snapshot()
	out := map[string]any{
		"reindexed": rec.Reindexed,
		"added":     rec.Added,
		"changed":   rec.Changed,
		"removed":   rec.Removed,
		"nodes":     g.NodeCount(),
		"edges":     g.EdgeCount(),
		"ms":        rec.Dur.Milliseconds(),
	}
	// Refresh the source-code index too, so one mesh_reindex catches both note and
	// code edits. ok=false means code indexing is not enabled for this vault.
	if cs, ok, cerr := s.reindexCode(); ok && cerr == nil {
		out["code_files"] = cs.Files
		out["code_symbols"] = cs.Symbols
		out["code_edges"] = cs.Edges
	}
	return textResult(out), nil
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
	cards, err := retriever.Retrieve(ctx, a.Query, retrieve.Options{Limit: a.Limit, Budget: a.Budget})
	if err != nil {
		return nil, internalErr(err)
	}
	return textResult(map[string]any{"cards": cards, "tokens": retrieve.TotalTokens(cards)}), nil
}

func (s *Server) toolFetch(raw json.RawMessage) (any, *rpcError) {
	var a struct {
		ID     string `json:"id"`
		Anchor string `json:"anchor"`
	}
	json.Unmarshal(raw, &a)
	rel, err := s.store.NotePath(a.ID)
	if err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown note id", Data: a.ID}
	}
	data, err := os.ReadFile(filepath.Join(s.vaultRoot, rel))
	if err != nil {
		return nil, internalErr(err)
	}
	body := string(data)
	if a.Anchor != "" {
		body = sectionByAnchor(body, a.Anchor)
	}
	return rawText(body), nil
}

func (s *Server) toolGodNodes(raw json.RawMessage) (any, *rpcError) {
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
	var hubs []hub
	for _, n := range g.Nodes() {
		if n.Kind != "note" {
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

func (s *Server) toolChangedSince(raw json.RawMessage) (any, *rpcError) {
	var a struct {
		Since int64 `json:"since"`
	}
	json.Unmarshal(raw, &a)
	refs, err := s.store.ChangedSince(a.Since)
	if err != nil {
		return nil, internalErr(err)
	}
	return textResult(map[string]any{"changed": refs}), nil
}

func (s *Server) toolWrite(raw json.RawMessage, forceType string) (any, *rpcError) {
	var a struct {
		Type     string   `json:"type"`
		Title    string   `json:"title"`
		Do       string   `json:"do"`
		Dont     string   `json:"dont"`
		Why      string   `json:"why"`
		Related  []string `json:"related"`
		Tags     []string `json:"tags"`
		Status   string   `json:"status"`
		Severity string   `json:"severity"`
	}
	json.Unmarshal(raw, &a)
	t := a.Type
	if forceType != "" {
		t = forceType
	}
	if t == "" {
		t = "note"
	}
	res, err := vault.CreateNote(s.vaultRoot, vault.NewNoteSpec{
		Type: vault.NoteType(t), Title: a.Title, Do: a.Do, Dont: a.Dont, Why: a.Why,
		Related: a.Related, Tags: a.Tags, Status: a.Status, Severity: a.Severity,
	})
	if err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: err.Error()}
	}
	if err := s.reload(); err != nil {
		return nil, internalErr(err)
	}
	return textResult(map[string]any{"id": res.ID, "path": res.Path, "when": res.When, "todo": res.TODOs}), nil
}

// sectionByAnchor returns the markdown of the heading section whose slug matches
// anchor (from that heading until the next heading of the same or higher level).
func sectionByAnchor(body, anchor string) string {
	lines := strings.Split(body, "\n")
	start, level := -1, 0
	for i, ln := range lines {
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
	for i := start + 1; i < len(lines); i++ {
		ln := lines[i]
		h := strings.TrimLeft(ln, "#")
		lvl := len(ln) - len(h)
		if lvl >= 1 && lvl <= level && strings.HasPrefix(h, " ") {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
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
