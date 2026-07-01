// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"net/http"
	"sort"
)

// handleDashboard returns the ROI + knowledge-health snapshot the Dashboard view
// renders: usage counters, an estimated token saving vs naive RAG, coverage by
// type, the most-reused notes, a contributor leaderboard (from provenance), and
// the lifecycle health counts. All local, read from the index.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	queries, _ := s.store.Metric("queries")
	fetches, _ := s.store.Metric("fetches")
	writes, _ := s.store.Metric("writes")
	notes, _ := s.store.Count("notes")

	// Estimated tokens saved vs a naive whole-file RAG dump. Mesh returns budgeted
	// cards (~1.9x fewer tokens per the 2026-07-02 benchmark, a median saving of
	// ~3,100-4,500 tokens/query); we credit a deliberately conservative 1800 tokens
	// per served query, well under the measured saving. Labeled an estimate in the UI.
	const tokensSavedPerQuery = 1800
	estTokensSaved := queries * tokensSavedPerQuery

	byType, _ := s.store.NotesByType()
	top, _ := s.store.TopFetched(8)
	health, _ := s.store.HealthCounts()
	flywheel, _ := s.store.FlywheelStats()
	pending, _ := s.store.PendingCount()

	// Contributor leaderboard (top 8 by authored notes).
	contribMap, _ := s.store.ContributorCounts()
	type kv struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	contrib := make([]kv, 0, len(contribMap))
	for n, c := range contribMap {
		contrib = append(contrib, kv{Name: n, Count: c})
	}
	sort.Slice(contrib, func(i, j int) bool { return contrib[i].Count > contrib[j].Count })
	if len(contrib) > 8 {
		contrib = contrib[:8]
	}

	writeJSON(w, map[string]any{
		"usage": map[string]any{
			"queries": queries, "fetches": fetches, "writes": writes, "notes": notes,
		},
		"est_tokens_saved":       estTokensSaved,
		"tokens_saved_per_query": tokensSavedPerQuery,
		"coverage":               byType,
		"top_fetched":            top,
		"contributors":           contrib,
		"health":                 health,
		"flywheel":               flywheel,
		"pending_review":         pending,
	})
}
