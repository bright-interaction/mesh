package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/brightinteraction/mesh/internal/retrieve"
	"github.com/brightinteraction/mesh/internal/vault"
)

func obj(m map[string]any) map[string]any { return m }

func (s *Server) handleToolsList() any {
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
	}
	return map[string]any{"tools": tools}
}

func (s *Server) handleToolsCall(params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "bad params"}
	}
	switch p.Name {
	case "mesh_search":
		return s.toolSearch(p.Arguments)
	case "mesh_fetch":
		return s.toolFetch(p.Arguments)
	case "mesh_god_nodes":
		return s.toolGodNodes(p.Arguments)
	case "mesh_changed_since":
		return s.toolChangedSince(p.Arguments)
	case "mesh_append_note":
		return s.toolWrite(p.Arguments, "")
	case "mesh_write_entity":
		return s.toolWrite(p.Arguments, "entity")
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "unknown tool", Data: p.Name}
	}
}

func (s *Server) toolSearch(raw json.RawMessage) (any, *rpcError) {
	var a struct {
		Query  string `json:"query"`
		Budget int    `json:"budget"`
		Limit  int    `json:"limit"`
	}
	json.Unmarshal(raw, &a)
	if strings.TrimSpace(a.Query) == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "query is required"}
	}
	cards, err := s.retriever.Retrieve(a.Query, retrieve.Options{Limit: a.Limit, Budget: a.Budget})
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
	var hubs []hub
	for _, n := range s.graph.Nodes() {
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
