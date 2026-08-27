// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/bright-interaction/mesh/internal/ask"
	"github.com/bright-interaction/mesh/internal/llm"
	"github.com/bright-interaction/mesh/internal/mcp"
)

// askMaxInFlight caps concurrent POST /api/ask requests. Each one forks a `claude -p`
// subprocess (default BYOAI backend) or bills the operator's LLM key, so without a cap
// any member, viewers included, can turn a request loop into unbounded process/fd/pid
// growth on the internet-facing app.
const askMaxInFlight = 3

// askMaxDuration bounds one answer. The CLI backend's own timeout is 300s, far longer
// than a human waits, and every second of it is a held subprocess and a held slot.
const askMaxDuration = 90 * time.Second

// askKey is the rate-limit key for POST /api/ask: the member id in member mode (so one
// member cannot spend the whole team's budget, and so a NAT full of teammates is not one
// bucket), the peer address otherwise.
func (s *Server) askKey(r *http.Request) string {
	if s.member != nil {
		if id, ok := s.member.clientFromRequest(r); ok {
			return "member:" + strconv.FormatInt(id, 10)
		}
	}
	return "peer:" + peerKey(r)
}

// handleAsk answers a natural-language question from the vault (notes + code) via the
// BYOAI LLM, grounded with citations and scoped to the member's readable notes. The LLM
// is BYOAI (MESH_CURATOR_*, default claude -p): if it is not available on this host the
// endpoint degrades gracefully rather than erroring, so the rest of the app is fine.
//
// Three guards, because this is the most expensive route in the app and memberGuard
// alone lets any viewer reach it: a per-caller token bucket, a bounded in-flight
// semaphore, and a hard deadline on the LLM call.
func (s *Server) handleAsk(w http.ResponseWriter, r *http.Request) {
	key := s.askKey(r)
	if !s.asks.allow(key) {
		slog.Warn("mesh ui: ask rate limit hit", "caller", key)
		w.Header().Set("Retry-After", "10")
		http.Error(w, "too many questions, slow down", http.StatusTooManyRequests)
		return
	}
	// Non-blocking acquire: queueing here would just move the exhaustion into pending
	// requests, so a full house is a 429 the caller can retry. Guarded on nil because a
	// send to a nil channel always falls to default, which would 429 forever on a Server
	// that was built without NewServer.
	if s.askSlots != nil {
		select {
		case s.askSlots <- struct{}{}:
			defer func() { <-s.askSlots }()
		default:
			slog.Warn("mesh ui: ask in-flight cap reached", "caller", key, "cap", askMaxInFlight)
			w.Header().Set("Retry-After", "10")
			http.Error(w, "the assistant is busy, retry shortly", http.StatusTooManyRequests)
			return
		}
	}
	var req struct {
		Question string `json:"question"`
		Budget   int    `json:"budget"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// The third caller of retrieval that takes free text, so it takes the same text
	// bound as mesh_search and /api/search. The 16 KiB body limit above bounds the
	// request, not the question.
	if len(req.Question) > mcp.SearchQueryMaxBytes {
		http.Error(w, fmt.Sprintf("question is too long: %d bytes, maximum %d. Ask in a sentence or two.", len(req.Question), mcp.SearchQueryMaxBytes), http.StatusBadRequest)
		return
	}
	client, err := llm.NewFromEnv()
	if err != nil {
		writeJSON(w, map[string]any{"answer": "Ask is not configured on this server. Set MESH_CURATOR_AGENT/MESH_CURATOR_CMD (or an LLM key) to enable it.", "citations": nil})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), askMaxDuration)
	defer cancel()
	rt, err := s.retrieverContext(ctx)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		writeJSON(w, map[string]any{"answer": "Could not answer right now: " + err.Error(), "citations": nil})
		return
	}
	res, err := ask.Answer(ctx, rt, s.store, client, req.Question, req.Budget, s.allowedScopes(r), s.allowedPath(r))
	if err != nil {
		// A runtime LLM failure (e.g. no claude on this host) degrades to a message, not a 500.
		writeJSON(w, map[string]any{"answer": "Could not answer right now: " + err.Error(), "citations": nil})
		return
	}
	writeJSON(w, res)
}
