// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

// Package rerank is the BYOAI cross-encoder layer. Like embeddings, Mesh never
// runs inference itself: it POSTs (query, candidate documents) to an endpoint
// the operator controls and gets back a relevance score per document. A
// cross-encoder reads the query and a document jointly, so it is a sharper
// relevance signal than the bi-encoder vector cosine - the lever for top-1
// precision (answer@1). It is a mechanical scoring transform, not reasoning AI;
// the agent is still the librarian. The endpoint can be fully local and
// sovereign (a self-hosted ONNX cross-encoder, see tools/rerank-server) or any
// cloud rerank API - the wire format is the same.
package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/safehttp"
)

// maxRerankResponseBytes bounds a rerank endpoint's response (a small score list);
// the operator-configurable endpoint must not be able to OOM the process.
const maxRerankResponseBytes = 16 << 20

// Result is one document's rerank score. Index is the document's position in the
// slice passed to Rerank; Score is the cross-encoder relevance (higher = more
// relevant). Scores are not normalized to any fixed range across models, so
// callers should rank by order or min-max normalize, never compare raw scores
// across endpoints.
type Result struct {
	Index int
	Score float64
}

// Reranker scores candidate documents against a query. Implementations must be
// deterministic for a given model so retrieval stays reproducible.
type Reranker interface {
	Rerank(ctx context.Context, query string, docs []string) ([]Result, error)
	Model() string
}

// HTTP speaks the de-facto-standard rerank contract shared by Cohere (/v2/rerank),
// Jina (/v1/rerank), Voyage, and self-hosted servers:
//
//	POST {Endpoint}  {"model","query","documents":[...],"top_n"?}
//	-> {"results":[{"index","relevance_score"}, ...]}  (sorted desc by score)
//
// Endpoint is the full POST URL (not a base), because the path differs per
// provider. Results are returned in the request's document order regardless of
// the response order, so the caller can index straight back into its candidates.
type HTTP struct {
	Endpoint string
	ModelID  string
	Key      string // bearer token; empty for keyless local endpoints
	Client   *http.Client
}

func NewHTTP(endpoint, model, key string) *HTTP {
	return &HTTP{
		Endpoint: strings.TrimRight(endpoint, "/"),
		ModelID:  model,
		Key:      key,
		Client:   safehttp.LLMClient(60 * time.Second),
	}
}

func (h *HTTP) Model() string { return h.ModelID }

func (h *HTTP) Rerank(ctx context.Context, query string, docs []string) ([]Result, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	payload := map[string]any{"model": h.ModelID, "query": query, "documents": docs, "top_n": len(docs)}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if h.Key != "" {
		req.Header.Set("Authorization", "Bearer "+h.Key)
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rerank endpoint %s: status %d", h.Endpoint, resp.StatusCode)
	}
	var out struct {
		Results []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRerankResponseBytes)).Decode(&out); err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(out.Results))
	seen := make([]bool, len(docs))
	for _, r := range out.Results {
		if r.Index < 0 || r.Index >= len(docs) {
			return nil, fmt.Errorf("rerank: result index %d out of range for %d docs", r.Index, len(docs))
		}
		if seen[r.Index] {
			return nil, fmt.Errorf("rerank: duplicate result index %d", r.Index)
		}
		seen[r.Index] = true
		results = append(results, Result{Index: r.Index, Score: r.RelevanceScore})
	}
	// Require a full permutation: a partial result set (some docs unscored) would
	// silently corrupt the caller's per-candidate scoring, so fail loudly instead
	// and let the caller degrade to the fused order.
	if len(results) != len(docs) {
		return nil, fmt.Errorf("rerank: got %d scores for %d docs", len(results), len(docs))
	}
	// Normalize to request order so callers can map straight back to candidates.
	sort.Slice(results, func(i, j int) bool { return results[i].Index < results[j].Index })
	return results, nil
}

// Stub is a deterministic, network-free reranker for tests and offline demos. It
// scores by query/document token overlap (Jaccard-ish). It is NOT a real
// cross-encoder: it exercises the rerank plumbing (ordering, integration,
// budget) but never proves retrieval quality.
type Stub struct{ M string }

func (s Stub) Model() string {
	if s.M != "" {
		return s.M
	}
	return "stub-overlap"
}

func (s Stub) Rerank(_ context.Context, query string, docs []string) ([]Result, error) {
	q := tokenSet(query)
	out := make([]Result, len(docs))
	for i, d := range docs {
		ds := tokenSet(d)
		hits := 0
		for t := range q {
			if ds[t] {
				hits++
			}
		}
		score := 0.0
		if len(q) > 0 {
			score = float64(hits) / float64(len(q))
		}
		out[i] = Result{Index: i, Score: score}
	}
	return out, nil
}

func tokenSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, t := range strings.Fields(strings.ToLower(s)) {
		out[strings.Trim(t, ".,:;!?()[]{}\"'`")] = true
	}
	delete(out, "")
	return out
}
