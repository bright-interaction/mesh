// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"encoding/json"
	"net/http"

	"github.com/bright-interaction/mesh/internal/ask"
	"github.com/bright-interaction/mesh/internal/llm"
)

// handleAsk answers a natural-language question from the vault (notes + code) via the
// BYOAI LLM, grounded with citations and scoped to the member's readable notes. The LLM
// is BYOAI (MESH_CURATOR_*, default claude -p): if it is not available on this host the
// endpoint degrades gracefully rather than erroring, so the rest of the app is fine.
func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Question string `json:"question"`
		Budget   int    `json:"budget"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	client, err := llm.NewFromEnv()
	if err != nil {
		writeJSON(w, map[string]any{"answer": "Ask is not configured on this server. Set MESH_CURATOR_AGENT/MESH_CURATOR_CMD (or an LLM key) to enable it.", "citations": nil})
		return
	}
	res, err := ask.Answer(r.Context(), s.retriever(), s.store, client, req.Question, req.Budget, s.allowedScopes(r))
	if err != nil {
		// A runtime LLM failure (e.g. no claude on this host) degrades to a message, not a 500.
		writeJSON(w, map[string]any{"answer": "Could not answer right now: " + err.Error(), "citations": nil})
		return
	}
	writeJSON(w, res)
}
