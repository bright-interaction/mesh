package web

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bright-interaction/mesh/internal/retrieve"
)

// handleSearch runs the same fused retrieval the agent gets over MCP and returns
// ranked cards, so a human can search the vault from the browser. The retriever is
// built per request from the current config (so a Settings change takes effect on
// the next search) over the latest graph.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Error(w, "q is required", http.StatusBadRequest)
		return
	}
	limit := atoiOr(r.URL.Query().Get("limit"), 12)
	budget := atoiOr(r.URL.Query().Get("budget"), 0)
	s.mu.RLock()
	g := s.graph
	s.mu.RUnlock()
	rt := retrieve.NewFromEnv(s.store, g)
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
	// html is server-rendered (gomarkdown, same as docs) so every reader shows nicely
	// formatted prose instead of raw markdown; markdown is kept for any raw consumer.
	writeJSON(w, map[string]any{"id": id, "path": rel, "markdown": string(data), "html": renderMD(data)})
}

// scopeIntersect reports whether a note's scopes intersect the allowed set. Empty
// scopes = the dev fail-safe default.
func scopeIntersect(scopes []string, allowed map[string]bool) bool {
	if len(scopes) == 0 {
		return allowed["dev"]
	}
	for _, s := range scopes {
		if allowed[s] {
			return true
		}
	}
	return false
}

func atoiOr(s string, def int) int {
	if v, err := strconv.Atoi(s); err == nil && v > 0 {
		return v
	}
	return def
}
