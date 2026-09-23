// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bright-interaction/mesh/internal/meshcfg"
	"github.com/bright-interaction/mesh/internal/retrieve"
	"github.com/bright-interaction/mesh/internal/vault"
)

const (
	batchFetchMaxItems  = 16
	batchFetchFileBytes = 1 << 20
	batchFetchTextBytes = 128 << 10
	batchFetchTimeout   = 5 * time.Second
)

type batchFetchItem struct {
	ID     string `json:"id"`
	Anchor string `json:"anchor,omitempty"`
}

type batchFetchGroup struct {
	ID      string
	Anchors []string
	Indices []int
}

type batchFetchResult struct {
	ID      string   `json:"id"`
	Indices []int    `json:"indices"`
	Anchors []string `json:"anchors,omitempty"`
	Status  string   `json:"status"`
	Text    string   `json:"text,omitempty"`
	path    string
}

type batchFetchResponse struct {
	Results []batchFetchResult `json:"results"`
	Omitted []int              `json:"omitted"`
	Tokens  int                `json:"tokens"`
	Workers int                `json:"workers_used"`
}

func batchFetchGroups(items []batchFetchItem) []batchFetchGroup {
	var groups []batchFetchGroup
	byID := map[string]int{}
	for i, item := range items {
		j, ok := byID[item.ID]
		if !ok {
			j = len(groups)
			byID[item.ID] = j
			groups = append(groups, batchFetchGroup{ID: item.ID})
		}
		g := &groups[j]
		g.Indices = append(g.Indices, i)
		found := false
		for _, anchor := range g.Anchors {
			found = found || anchor == item.Anchor
		}
		if !found {
			g.Anchors = append(g.Anchors, item.Anchor)
		}
	}
	// A full-note request subsumes section requests for that same note.
	for i := range groups {
		for _, anchor := range groups[i].Anchors {
			if anchor == "" {
				groups[i].Anchors = []string{""}
				break
			}
		}
	}
	return groups
}

// There is no producer goroutine and no persistent pool. Every request joins
// every worker before returning, including after cancellation or a read error.
// Filesystem operations are cooperatively cancellable between reads, not forcibly
// interrupted; Wait prevents any in-flight operation from outliving the request.
func runBatchFetch(ctx context.Context, groups []batchFetchGroup, workers int, fetch func(context.Context, batchFetchGroup) batchFetchResult) ([]batchFetchResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if workers < 1 {
		workers = 1
	}
	if workers > len(groups) {
		workers = len(groups)
	}
	results := make([]batchFetchResult, len(groups))
	var next atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				j := int(next.Add(1) - 1)
				if j >= len(groups) {
					return
				}
				if ctx.Err() != nil {
					return
				}
				results[j] = fetch(ctx, groups[j])
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func (s *Server) toolFetchMany(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Items  []batchFetchItem `json:"items"`
		Budget int              `json:"budget"`
	}
	if len(raw) > 16384 || json.Unmarshal(raw, &args) != nil || len(args.Items) == 0 || len(args.Items) > batchFetchMaxItems {
		return nil, &rpcError{Code: codeInvalidParams, Message: "provide 1-16 note/anchor items"}
	}
	for _, item := range args.Items {
		if strings.TrimSpace(item.ID) == "" || len(item.ID) > 256 || len(item.Anchor) > 256 {
			return nil, &rpcError{Code: codeInvalidParams, Message: "note ids and anchors must be at most 256 bytes; ids cannot be empty"}
		}
	}
	if args.Budget == 0 {
		args.Budget = 8000
	}
	if args.Budget < 256 || args.Budget > 32000 {
		return nil, &rpcError{Code: codeInvalidParams, Message: "budget must be 256-32000 response tokens"}
	}
	ctx, cancel := context.WithTimeout(ctx, batchFetchTimeout)
	defer cancel()
	workers, err := meshcfg.FetchWorkersContext(ctx, s.store.MeshDir())
	if err != nil {
		return nil, &rpcError{Code: codeInternalError, Message: "invalid or unavailable fetch-worker setting; use retrieval.fetch_workers or MESH_FETCH_WORKERS with an integer from 1 to 16"}
	}
	groups := batchFetchGroups(args.Items)
	workers = min(workers, len(groups))
	results, err := runBatchFetch(ctx, groups, workers, s.fetchBatchGroup)
	if err != nil {
		return nil, &rpcError{Code: codeInternalError, Message: "batch fetch canceled or timed out"}
	}
	// Most batches fit. Price the complete response first rather than repeatedly
	// serializing/tokenizing every growing prefix (quadratic in batch size).
	response := batchFetchResponse{Results: results, Omitted: []int{}, Workers: workers}
	out, fits := priceBatchFetch(&response, args.Budget)
	if !fits {
		response = batchFetchResponse{Results: []batchFetchResult{}, Omitted: make([]int, len(args.Items)), Workers: workers}
		for i := range args.Items {
			response.Omitted[i] = i
		}
		// Always price the entire MCP content result, including omitted indices,
		// result JSON, escaping and the token receipt itself. No safety-text slicing.
		if _, ok := priceBatchFetch(&response, args.Budget); !ok {
			return nil, &rpcError{Code: codeInvalidParams, Message: "budget is too small for the batch receipt"}
		}
		for _, result := range results {
			if ctx.Err() != nil {
				return nil, &rpcError{Code: codeInternalError, Message: "batch fetch canceled or timed out"}
			}
			candidate := batchFetchResponse{Results: append(append([]batchFetchResult{}, response.Results...), result), Omitted: []int{}, Workers: workers}
			for _, i := range response.Omitted {
				found := false
				for _, j := range result.Indices {
					found = found || i == j
				}
				if !found {
					candidate.Omitted = append(candidate.Omitted, i)
				}
			}
			if _, ok := priceBatchFetch(&candidate, args.Budget); ok {
				response = candidate
			}
		}
		out, fits = priceBatchFetch(&response, args.Budget)
		if !fits {
			return nil, &rpcError{Code: codeInternalError, Message: "batch budget accounting failed"}
		}
	}
	if ctx.Err() != nil {
		return nil, &rpcError{Code: codeInternalError, Message: "batch fetch canceled or timed out"}
	}
	// Attribution follows returned input order, never worker completion order.
	// Omitted/error/duplicate items do not count as additional reuse.
	for _, result := range response.Results {
		if result.Status == "ok" {
			s.recordFetch(ctx, result.ID, result.path)
		}
	}
	return out, nil
}

func priceBatchFetch(response *batchFetchResponse, budget int) (any, bool) {
	// Monotonic receipt refinement avoids a digit-tokenization oscillation.
	for i := 0; i < 16; i++ {
		body, _ := json.Marshal(response)
		out := rawText(string(body))
		wire, _ := json.Marshal(out)
		tokens := retrieve.EstimateTokens(string(wire))
		if tokens > budget {
			return nil, false
		}
		if tokens <= response.Tokens {
			return out, response.Tokens <= budget
		}
		response.Tokens = tokens
	}
	return nil, false
}

func (s *Server) fetchBatchGroup(ctx context.Context, g batchFetchGroup) batchFetchResult {
	result := batchFetchResult{ID: g.ID, Indices: g.Indices, Anchors: g.Anchors, Status: "unavailable"}
	if ctx.Err() != nil {
		return result
	}
	metadata, err := s.store.NoteMetadataFor(ctx, []string{"note:" + g.ID})
	if err != nil {
		return result
	}
	m, ok := metadata["note:"+g.ID]
	sf := scopeFromCtx(ctx)
	if !ok || (sf != nil && !vault.ScopeAllowsCSV(m.Scope, sf.AllowedRead)) || !filepath.IsLocal(m.Path) {
		return result
	}
	body, err := readBatchFile(ctx, s.vaultRoot, m.Path)
	if err != nil {
		if errors.Is(err, errBatchFileTooLarge) {
			result.Status = "too_large"
		}
		return result
	}
	if ctx.Err() != nil {
		return result
	}
	// Require BOTH persisted authorization and current frontmatter, just as
	// single-note fetch does. Neither surface may become a bypass for the other.
	if !fetchFileScopeAllowed(body, sf) {
		return result
	}
	text, rerr := s.formatFetchDocument(ctx, g.ID, m.Path, string(body), g.Anchors)
	if rerr != nil {
		return result
	} // opaque: never leak internal paths or denied ids
	if len(text) > batchFetchTextBytes {
		result.Status = "too_large"
		return result
	}
	result.Status, result.Text, result.path = "ok", text, m.Path
	return result
}

var errBatchFileTooLarge = vault.ErrConfinedFileTooLarge

func readBatchFile(ctx context.Context, root, rel string) ([]byte, error) {
	return readFetchFile(ctx, root, rel, batchFetchFileBytes)
}

// readFetchFile confines reads to held vault directory handles and refuses
// symlinks/non-regular files. maxBytes=0 retains single-fetch size compatibility;
// batch fetch keeps its existing byte limit. No detached read worker is started.
func readFetchFile(ctx context.Context, root, rel string, maxBytes int) ([]byte, error) {
	return vault.ReadConfinedFile(ctx, root, rel, maxBytes)
}
