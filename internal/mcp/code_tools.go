package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/meshcfg"
)

// toolCodeSearch is the graphify replacement: FTS over the source-code symbol
// index, ranked by name match. It returns symbol cards with a file:line locator so
// an agent can jump straight to a definition instead of grepping the tree. It is
// deliberately separate from mesh_search so locating a function never competes with
// note retrieval or the tier-0 budget.
func (s *Server) toolCodeSearch(raw json.RawMessage) (any, *rpcError) {
	var a struct {
		Query     string   `json:"query"`
		Limit     int      `json:"limit"`
		Languages []string `json:"languages"`
	}
	json.Unmarshal(raw, &a)
	if strings.TrimSpace(a.Query) == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "query required"}
	}
	hits, err := s.store.SearchCode(a.Query, a.Limit, a.Languages)
	if err != nil {
		return nil, internalErr(err)
	}
	cards := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		cards = append(cards, map[string]any{
			"id": h.ID, "name": h.Name, "kind": h.Kind, "lang": h.Lang,
			"loc": fmt.Sprintf("%s:%d", h.Path, h.Line), "path": h.Path, "line": h.Line,
			"signature": h.Signature, "snippet": h.Snippet, "score": h.Score,
		})
	}
	out := map[string]any{"symbols": cards, "count": len(cards)}
	if len(cards) == 0 && !s.store.CodeIndexed() {
		out["note"] = "the source-code index is empty; run `mesh code reindex` or enable [code] in .mesh/config.toml"
	}
	return textResult(out), nil
}

// toolCodeNeighbors returns the call-graph neighbors of a symbol id: what it calls
// (callees) and what calls it (callers). Edges exist for Go; other languages return
// empty lists (the declaration scanner locates symbols but does not trace calls).
func (s *Server) toolCodeNeighbors(raw json.RawMessage) (any, *rpcError) {
	var a struct {
		ID string `json:"id"`
	}
	json.Unmarshal(raw, &a)
	if strings.TrimSpace(a.ID) == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "id required"}
	}
	callers, callees, err := s.store.CodeNeighbors(a.ID)
	if err != nil {
		return nil, internalErr(err)
	}
	return textResult(map[string]any{
		"id":      a.ID,
		"callers": codeRefCards(callers),
		"callees": codeRefCards(callees),
	}), nil
}

func codeRefCards(refs []index.CodeRef) []map[string]any {
	out := make([]map[string]any, 0, len(refs))
	for _, r := range refs {
		out = append(out, map[string]any{
			"id": r.ID, "name": r.Name, "kind": r.Kind,
			"loc": fmt.Sprintf("%s:%d", r.Path, r.Line), "path": r.Path, "line": r.Line,
			"signature": r.Signature,
		})
	}
	return out
}

// reindexCode refreshes the source-code index from the configured roots, used by
// mesh_reindex so an agent can force a code refresh the same way it forces a note
// refresh. Returns ok=false (no error) when code indexing is not enabled. Env
// MESH_CODE_ROOTS / MESH_CODE_INDEX override the config file.
func (s *Server) reindexCode() (stats index.CodeStats, ok bool, err error) {
	cfg, err := meshcfg.LoadConfig(s.store.MeshDir())
	if err != nil {
		return index.CodeStats{}, false, err
	}
	on := cfg.Code.Index || os.Getenv("MESH_CODE_INDEX") == "1"
	roots := cfg.Code.Roots
	if env := os.Getenv("MESH_CODE_ROOTS"); env != "" {
		roots = splitCSV(env)
		on = true
	}
	if !on || len(roots) == 0 {
		return index.CodeStats{}, false, nil
	}
	stats, err = index.ReindexCode(s.store, roots, langSet(cfg.Code.Languages))
	return stats, true, err
}

func langSet(langs []string) map[string]bool {
	if len(langs) == 0 {
		return nil
	}
	m := make(map[string]bool, len(langs))
	for _, l := range langs {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" {
			m[l] = true
		}
	}
	return m
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
