// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bright-interaction/mesh/internal/retrieve"
	"github.com/bright-interaction/mesh/internal/vault"
)

// handleSearch runs the same fused retrieval the agent gets over MCP and returns
// ranked cards, so a human can search the vault from the browser. The retriever is
// cached (built lazily, invalidated on reindex/config change) instead of rebuilt per
// request, so a search no longer pays a full LoadVectors + ANN rebuild every time.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Error(w, "q is required", http.StatusBadRequest)
		return
	}
	limit := atoiOr(r.URL.Query().Get("limit"), 12)
	budget := atoiOr(r.URL.Query().Get("budget"), 0)
	rt := s.retriever()
	cards, err := rt.Retrieve(r.Context(), q, retrieve.Options{Limit: limit, Budget: budget, AllowedScopes: s.allowedScopes(r)})
	if err != nil {
		http.Error(w, "search failed", http.StatusInternalServerError)
		return
	}
	_ = s.store.IncrMetric("queries", 1) // ROI telemetry (best-effort)
	writeJSON(w, map[string]any{"cards": cards, "tokens": retrieve.TotalTokens(cards)})
}

// handleNote returns one note's raw markdown by frontmatter id, the browser
// equivalent of mesh_fetch. Path is resolved through the index (id -> rel path),
// never from client input, so it cannot escape the vault.
func (s *Server) handleNote(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rel, err := s.store.NotePath(id)
	if err != nil {
		http.Error(w, "unknown note id", http.StatusNotFound)
		return
	}
	// Scope read check: opaque 404 (same as a missing note) so a scoped member cannot
	// probe which ids exist outside their scope.
	if allowed := s.allowedScopes(r); allowed != nil {
		sc, serr := s.store.NoteScope(id)
		if serr != nil || !scopeIntersect(sc, allowed) {
			http.Error(w, "unknown note id", http.StatusNotFound)
			return
		}
	}
	data, err := os.ReadFile(filepath.Join(s.vaultRoot, rel))
	if err != nil {
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}
	// html is server-rendered (gomarkdown) so every reader shows formatted prose. Note
	// bodies are UNTRUSTED (ingested connector content can carry raw HTML), so render
	// with the sanitising path; markdown is kept verbatim for any raw consumer.
	writeJSON(w, map[string]any{"id": id, "path": rel, "markdown": string(data), "html": renderMDSafe(data)})
}

// scopeIntersect reports whether a note's scopes intersect the allowed set. Empty
// scopes = the dev fail-safe default. Delegates to the one shared predicate so this
// surface cannot drift from the MCP/retrieve scope checks.
func scopeIntersect(scopes []string, allowed map[string]bool) bool {
	return vault.ScopeAllows(scopes, allowed)
}

func atoiOr(s string, def int) int {
	if v, err := strconv.Atoi(s); err == nil && v > 0 {
		return v
	}
	return def
}
