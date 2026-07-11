// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/meshcfg"
)

// codeScopeDenied reports whether a scope-confined caller must be denied the
// source-code index. Code symbols carry no per-note scope, so the whole index is
// unscoped (dev-scoped) content, exactly like a note with no scope: a caller who
// cannot read the dev scope must not be able to enumerate symbols, paths,
// signatures, snippets, or the call graph for code outside their scope. A nil filter
// (solo run, hub with no scoping, or an admin's unrestricted filter) denies nothing.
func codeScopeDenied(ctx context.Context) bool {
	sf := scopeFromCtx(ctx)
	return sf != nil && !sf.allowsRead(nil) // allowsRead(nil) => AllowedRead["dev"]
}

// toolCodeSearch is the graphify replacement: FTS over the source-code symbol
// index, ranked by name match. It returns symbol cards with a file:line locator so
// an agent can jump straight to a definition instead of grepping the tree. It is
// deliberately separate from mesh_search so locating a function never competes with
// note retrieval or the tier-0 budget.
func (s *Server) toolCodeSearch(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	if codeScopeDenied(ctx) {
		return textResult(map[string]any{
			"symbols": []map[string]any{}, "count": 0,
			"note": "the source-code index is not in your access scope",
		}), nil
	}
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
		card := map[string]any{
			"id": h.ID, "name": h.Name, "kind": h.Kind, "lang": h.Lang,
			"loc": fmt.Sprintf("%s:%d", h.Path, h.Line), "path": h.Path, "line": h.Line,
			"signature": h.Signature, "snippet": h.Snippet, "score": h.Score,
		}
		// Surface the note<->code bridge: if notes reference this symbol, hint that
		// mesh_code_context will return the institutional knowledge about it.
		if nc := s.store.NoteCountForSymbol(h.ID); nc > 0 {
			card["notes"] = nc
		}
		cards = append(cards, card)
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
func (s *Server) toolCodeNeighbors(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var a struct {
		ID string `json:"id"`
	}
	json.Unmarshal(raw, &a)
	if strings.TrimSpace(a.ID) == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "id required"}
	}
	if codeScopeDenied(ctx) {
		return textResult(map[string]any{
			"id": a.ID, "callers": []map[string]any{}, "callees": []map[string]any{},
			"note": "the source-code index is not in your access scope",
		}), nil
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

// toolCodeContext fuses the two indexes: it resolves a symbol like mesh_code_search,
// then for each match returns the NOTES that reference it (decisions/gotchas about that
// code). This is "what do we know about this function" - code plus the institutional
// knowledge around it, in one call. Linked notes are scope-filtered like any read.
func (s *Server) toolCodeContext(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	if codeScopeDenied(ctx) {
		return textResult(map[string]any{"symbols": []map[string]any{}, "count": 0,
			"note": "the source-code index is not in your access scope"}), nil
	}
	var a struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	json.Unmarshal(raw, &a)
	if strings.TrimSpace(a.Query) == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "query required"}
	}
	limit := a.Limit
	if limit <= 0 || limit > 10 {
		limit = 5
	}
	hits, err := s.store.SearchCode(a.Query, limit, nil)
	if err != nil {
		return nil, internalErr(err)
	}
	sf := scopeFromCtx(ctx)
	readable := func(noteID string) bool {
		if sf == nil {
			return true
		}
		sc, e := s.store.NoteScope(noteID)
		return e == nil && sf.allowsRead(sc)
	}
	cards := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		cards = append(cards, map[string]any{
			"id": h.ID, "name": h.Name, "kind": h.Kind, "lang": h.Lang,
			"loc": fmt.Sprintf("%s:%d", h.Path, h.Line), "signature": h.Signature,
		})
	}
	// Notes about anything matching the query name (type OR its methods), so a search
	// that lands on a method still surfaces notes filed against the type.
	notes, _ := s.store.NotesForSymbolName(a.Query)
	noteCards := make([]map[string]any, 0, len(notes))
	for _, nt := range notes {
		if !readable(nt.NoteID) {
			continue
		}
		noteCards = append(noteCards, map[string]any{
			"id": nt.NoteID, "title": nt.Title, "path": nt.Path, "type": nt.Type,
		})
	}
	return textResult(map[string]any{"symbols": cards, "notes": noteCards, "count": len(cards)}), nil
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
	if err == nil {
		_, _ = s.store.LinkNotesToCode(s.vaultRoot) // refresh the note<->code bridge (best-effort)
	}
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
