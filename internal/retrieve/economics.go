// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/rerank"
)

const defaultRerankConfidenceMargin = 0.45

// Economics is content-free accounting for one retrieval. It can safely be
// persisted as counters: it contains no query, title, path, snippet, or note id.
type Economics struct {
	SemanticConfigured   bool   `json:"semantic_configured"`
	SemanticFallback     bool   `json:"semantic_fallback,omitempty"`
	SemanticCircuitOpen  bool   `json:"semantic_circuit_open,omitempty"`
	RerankConfigured     bool   `json:"configured"`
	RerankModel          string `json:"model,omitempty"`
	Route                string `json:"route"`
	RerankCalled         bool   `json:"called"`
	CacheHit             bool   `json:"cache_hit,omitempty"`
	CircuitOpen          bool   `json:"circuit_open,omitempty"`
	Fallback             bool   `json:"fallback,omitempty"`
	CandidateCards       int    `json:"candidate_cards,omitempty"`
	RerankInput          int    `json:"rerank_input_tokens,omitempty"`
	RerankOutput         int    `json:"rerank_output_tokens,omitempty"`
	RerankProviderTokens int    `json:"provider_tokens,omitempty"`
	ProviderReported     bool   `json:"provider_reported,omitempty"`
	RerankLatencyMS      int64  `json:"rerank_latency_ms,omitempty"`
	LocalCards           int    `json:"local_cards,omitempty"`
	LocalCardTokens      int    `json:"local_card_tokens,omitempty"`
	ReturnedCards        int    `json:"returned_cards,omitempty"`
	ReturnedTokens       int    `json:"returned_tokens,omitempty"`
	SearchLatencyMS      int64  `json:"search_latency_ms,omitempty"`
}

// RerankReceipt is the deliberately tiny wire form returned beside search
// cards after an exceptional fallback. Detailed accounting stays in local
// counters; the calling agent only needs to know what failed over. Omitting the
// model name avoids paying for it in that response too.
type RerankReceipt struct {
	Route           string `json:"route"`
	Called          bool   `json:"called,omitempty"`
	CacheHit        bool   `json:"cache_hit,omitempty"`
	Fallback        bool   `json:"fallback,omitempty"`
	CircuitOpen     bool   `json:"circuit_open,omitempty"`
	AccountedTokens int    `json:"accounted_tokens,omitempty"`
}

// SemanticReceipt is emitted only when the optional vector lane could not run and
// retrieval continued with the local FTS + graph lanes. Keeping it this small makes
// the degradation visible without spending the token savings on diagnostic prose.
type SemanticReceipt struct {
	Route       string `json:"route"`
	Fallback    bool   `json:"fallback"`
	CircuitOpen bool   `json:"circuit_open,omitempty"`
}

func (e Economics) SemanticReceipt() SemanticReceipt {
	return SemanticReceipt{Route: "lexical_graph", Fallback: e.SemanticFallback, CircuitOpen: e.SemanticCircuitOpen}
}

func (e Economics) Receipt() RerankReceipt {
	return RerankReceipt{
		Route:           e.Route,
		Called:          e.RerankCalled,
		CacheHit:        e.CacheHit,
		Fallback:        e.Fallback,
		CircuitOpen:     e.CircuitOpen,
		AccountedTokens: e.ModelTokens(),
	}
}

// ModelTokens is the best available total for the second call.
func (e Economics) ModelTokens() int {
	if e.ProviderReported {
		return e.RerankProviderTokens
	}
	return e.RerankInput + e.RerankOutput
}

// ReceiptTokens prices the exceptional receipt before a wire surface packs
// cards. The wrapper is included deliberately, slightly over-pricing the comma
// used when it is merged into the complete response map.
func (e Economics) ReceiptTokens() int {
	if !e.Fallback && !e.SemanticFallback {
		return 0
	}
	receipts := make(map[string]any, 2)
	if e.Fallback {
		receipts["rerank"] = e.Receipt()
	}
	if e.SemanticFallback {
		receipts["semantic"] = e.SemanticReceipt()
	}
	b, err := json.Marshal(receipts)
	if err != nil {
		return 0
	}
	return EstimateTokens(string(b))
}

// RecordEconomics accumulates local monotonic counters. It is deliberately a
// separate call so each surface can set ReturnedTokens using its exact final
// wire representation before persisting the measurement.
func (r *Retriever) RecordEconomics(e Economics) {
	if r == nil || r.store == nil {
		return
	}
	inc := func(key string, n int64) {
		if n > 0 {
			_ = r.store.IncrMetric(key, n)
		}
	}
	inc("retrieval:searches", 1)
	inc("retrieval:returned_cards", int64(e.ReturnedCards))
	inc("retrieval:returned_tokens", int64(e.ReturnedTokens))
	inc("retrieval:latency_ms", e.SearchLatencyMS)
	if e.SemanticConfigured {
		inc("semantic:configured", 1)
	}
	if e.SemanticFallback {
		inc("semantic:fallbacks", 1)
	}
	if e.SemanticCircuitOpen {
		inc("semantic:circuit_open", 1)
	}
	if !e.RerankConfigured {
		inc("rerank:off", 1)
		return
	}
	inc("rerank:configured", 1)
	inc("rerank:local_context_tokens", int64(e.LocalCardTokens))
	inc("rerank:actual_context_tokens", int64(e.ReturnedTokens+e.ModelTokens()))
	inc("rerank:route:"+knownEconomicsRoute(e.Route), 1)
	inc("rerank:candidates", int64(e.CandidateCards))
	inc("rerank:input_tokens", int64(e.RerankInput))
	inc("rerank:output_tokens", int64(e.RerankOutput))
	inc("rerank:accounted_tokens", int64(e.ModelTokens()))
	inc("rerank:provider_tokens", int64(e.RerankProviderTokens))
	inc("rerank:latency_ms", e.RerankLatencyMS)
	if e.RerankCalled {
		inc("rerank:calls", 1)
	}
	if e.ProviderReported {
		inc("rerank:provider_reported_calls", 1)
	}
	if e.CacheHit {
		inc("rerank:cache_hits", 1)
	}
	if e.CircuitOpen {
		inc("rerank:circuit_open", 1)
	}
	if e.Fallback {
		inc("rerank:fallbacks", 1)
	}
}

func knownEconomicsRoute(route string) string {
	switch route {
	case "model", "cache", "local_exact", "local_confident", "fallback", "too_few", "disabled":
		return route
	default:
		return "other"
	}
}

func rerankPolicy(rr rerank.Reranker) string {
	if p, ok := rr.(rerank.PolicyReporter); ok {
		return p.RerankPolicy()
	}
	return "always"
}

// subscriptionRoute is intentionally conservative. Auto mode skips the model
// only for an exact identity/title lookup or when every meaningful query term is
// present in a clearly separated top FTS card. Everything ambiguous still gets
// the small-model judgment.
func subscriptionRoute(query string, cards []Card, policy string) (bool, string) {
	if policy == "always" {
		return true, "model"
	}
	if len(cards) < 2 {
		return false, "too_few"
	}
	q := normalizedLookup(query)
	if q != "" {
		c := cards[0]
		base := strings.TrimSuffix(filepath.Base(c.Path), filepath.Ext(c.Path))
		if q == normalizedLookup(c.NoteID) || q == normalizedLookup(c.Title) || q == normalizedLookup(base) || q == normalizedLookup(c.Path) {
			return false, "local_exact"
		}
	}

	terms := graph.TokenizeQuery(query)
	if len(terms) < 2 || cards[0].Reason != "fts" || cards[0].Score <= 0 {
		return true, "model"
	}
	topTerms := make(map[string]bool)
	for _, term := range graph.Tokenize(cards[0].Title + " " + cards[0].Snippet) {
		topTerms[term] = true
	}
	for _, term := range terms {
		if !topTerms[term] {
			return true, "model"
		}
	}
	margin := (cards[0].Score - cards[1].Score) / math.Max(math.Abs(cards[0].Score), 1e-9)
	if margin >= rerankConfidenceMargin() {
		return false, "local_confident"
	}
	return true, "model"
}

func normalizedLookup(s string) string {
	return strings.Join(graph.TokenizeQuery(strings.TrimPrefix(strings.TrimSpace(s), "note:")), " ")
}

func rerankConfidenceMargin() float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv("MESH_RERANK_CONFIDENCE_MARGIN")), 64)
	if err != nil || v < 0 || v > 1 {
		return defaultRerankConfidenceMargin
	}
	return v
}

func durationMillis(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	if d < time.Millisecond {
		return 1
	}
	return d.Milliseconds()
}

func estimateRerankResults(results []rerank.Result) int {
	b, err := json.Marshal(results)
	if err != nil {
		return 0
	}
	return EstimateTokens(string(b))
}
