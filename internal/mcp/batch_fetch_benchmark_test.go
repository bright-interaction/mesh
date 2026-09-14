// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/retrieve"
)

// These measurements include the HTTP handler, dispatch, reading/rendering,
// budget packing, attribution and final JSON serialization. They deliberately
// exclude sockets, authentication middleware and model turns. Test request
// encoding, response copying and lightweight envelope decoding are included.
type batchE2ECall struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
	Wire   []byte          `json:"-"`
}

func batchE2ERequest(s *Server, ctx context.Context, tool string, args any) (batchE2ECall, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		return batchE2ECall{}, err
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.HandleHTTP(rec, req)
	var out batchE2ECall
	if rec.Code != http.StatusOK {
		return out, fmt.Errorf("HTTP status %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		return out, err
	}
	out.Wire = bytes.Clone(rec.Body.Bytes())
	return out, nil
}

func batchE2EText(call batchE2ECall) (string, error) {
	if call.Error != nil {
		return "", fmt.Errorf("RPC error %d: %s", call.Error.Code, call.Error.Message)
	}
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(call.Result, &result); err != nil {
		return "", err
	}
	if len(result.Content) != 1 || result.Content[0].Type != "text" {
		return "", fmt.Errorf("unexpected content shape")
	}
	return result.Content[0].Text, nil
}

func batchE2EReady(s *Server) {
	s.ready = make(chan struct{})
	close(s.ready)
}

// Complete-result checks: each input is returned or explicitly omitted exactly
// once, with complete required facts and lifecycle restrictions in every result.
func batchE2ECheck(call batchE2ECall, items []batchFetchItem, budget int, denied map[string]bool) (batchFetchResponse, error) {
	text, err := batchE2EText(call)
	if err != nil {
		return batchFetchResponse{}, err
	}
	var result batchFetchResponse
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		return result, err
	}
	actual := retrieve.EstimateTokens(string(call.Result))
	if actual > result.Tokens || result.Tokens > budget {
		return result, fmt.Errorf("MCP result tokens=%d receipt=%d budget=%d", actual, result.Tokens, budget)
	}
	seen := make([]bool, len(items))
	for _, r := range result.Results {
		if len(r.Indices) == 0 {
			return result, fmt.Errorf("missing input indices")
		}
		for _, i := range r.Indices {
			if i < 0 || i >= len(items) || seen[i] || items[i].ID != r.ID {
				return result, fmt.Errorf("invalid or duplicate result index %d", i)
			}
			seen[i] = true
			if denied[r.ID] {
				if r.Status != "unavailable" || r.Text != "" {
					return result, fmt.Errorf("scope leak for %s", r.ID)
				}
				continue
			}
			if r.Status != "ok" {
				return result, fmt.Errorf("unexpected status %s for %s", r.Status, r.ID)
			}
			if err := batchE2ERequired(r.Text, items[i].Anchor); err != nil {
				return result, err
			}
		}
	}
	for _, i := range result.Omitted {
		if i < 0 || i >= len(items) || seen[i] {
			return result, fmt.Errorf("invalid or duplicate omission %d", i)
		}
		seen[i] = true
	}
	for i, ok := range seen {
		if !ok {
			return result, fmt.Errorf("input %d lost", i)
		}
	}
	return result, nil
}

func batchE2ERequired(text, anchor string) error {
	required := []string{"retired", "Do not deploy."}
	switch anchor {
	case "":
		required = append(required, "parent answer", "child answer", "other answer")
	case "parent":
		required = append(required, "parent answer", "child answer")
	case "child":
		required = append(required, "child answer")
	case "other":
		required = append(required, "other answer")
	default:
		return fmt.Errorf("test has no coverage oracle for %q", anchor)
	}
	for _, fact := range required {
		if !strings.Contains(text, fact) {
			return fmt.Errorf("missing required fact %q", fact)
		}
	}
	return nil
}

func TestBatchFetchHTTPConcurrentScopeBudgetAndCancellation(t *testing.T) {
	t.Setenv("MESH_FETCH_WORKERS", "4")
	s := batchFixture(t, 4)
	batchE2EReady(s)
	for _, i := range []int{1, 3} {
		path := filepath.Join(s.vaultRoot, fmt.Sprintf("n%d.md", i))
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, bytes.ReplaceAll(body, []byte("[public]"), []byte("[private]")), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := index.ReindexFull(s.store, s.vaultRoot); err != nil {
		t.Fatal(err)
	}
	items := []batchFetchItem{{"n0", "parent"}, {"n1", "child"}, {"n2", "other"}, {"n3", ""}, {"n0", "child"}}
	start := make(chan struct{})
	errs := make(chan error, 12)
	var wg sync.WaitGroup
	for caller := 0; caller < 12; caller++ {
		wg.Add(1)
		go func(caller int) {
			defer wg.Done()
			ctx := context.Background()
			denied := map[string]bool{}
			if caller%2 == 0 {
				ctx = WithScopeFilter(ctx, &ScopeFilter{AllowedRead: map[string]bool{"public": true}})
				denied = map[string]bool{"n1": true, "n3": true}
			} else {
				ctx = WithScopeFilter(ctx, &ScopeFilter{AllowedRead: map[string]bool{"private": true}})
				denied = map[string]bool{"n0": true, "n2": true}
			}
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			canceled := caller%3 == 0
			if canceled {
				cancel()
			}
			budget := 8000
			if caller%3 == 1 {
				budget = 256
			}
			<-start
			call, err := batchE2ERequest(s, ctx, "mesh_fetch_many", map[string]any{"items": items, "budget": budget})
			if err != nil {
				errs <- err
				return
			}
			if canceled {
				if call.Error == nil || len(call.Result) != 0 {
					errs <- fmt.Errorf("canceled caller %d got data", caller)
				}
				return
			}
			result, err := batchE2ECheck(call, items, budget, denied)
			if err == nil && ((budget == 8000 && len(result.Omitted) != 0) || (budget == 256 && len(result.Omitted) == 0)) {
				err = fmt.Errorf("unexpected omissions for budget %d", budget)
			}
			if err != nil {
				errs <- fmt.Errorf("caller %d: %w", caller, err)
			}
		}(caller)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

type batchE2EMetrics struct {
	Requests     int `json:"requests_per_journey"`
	ResultTokens int `json:"mcp_result_tokens"`
	WorkersUsed  int `json:"workers_used"`
	WireTokens   int `json:"jsonrpc_wire_tokens"`
	WireBytes    int `json:"jsonrpc_wire_bytes"`
	Omitted      int `json:"omitted_inputs"`
}

type batchE2EObservation struct {
	Workload string  `json:"workload"`
	Mode     string  `json:"mode"`
	Workers  int     `json:"workers"`
	Callers  int     `json:"concurrent_callers"`
	Samples  int     `json:"rounds"`
	MedianMS float64 `json:"round_median_ms"`
	P95MS    float64 `json:"round_p95_ms"`
	batchE2EMetrics
}

// TestBatchFetchHTTPEvaluation is explicitly opt-in so race suites and ordinary
// correctness tests do not produce noisy, machine-contended speed claims.
func TestBatchFetchHTTPEvaluation(t *testing.T) {
	if os.Getenv("MESH_BATCH_E2E_EVAL") != "1" {
		t.Skip("set MESH_BATCH_E2E_EVAL=1 for synthetic warm full-handler measurements")
	}
	const notes, rounds, warmups, budget = 8, 7, 2, 32000
	var observations []batchE2EObservation
	for _, workload := range []string{"small-whole", "larger-whole", "larger-nested-sections"} {
		s := batchFixture(t, notes)
		batchE2EReady(s)
		if workload != "small-whole" {
			for i := 0; i < notes; i++ {
				path := filepath.Join(s.vaultRoot, fmt.Sprintf("n%d.md", i))
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				// Padding lives outside selected parent/child sections. Whole-note
				// and section comparisons always request exactly the same inputs.
				body = append(body, []byte(strings.Repeat("reference detail alpha beta gamma delta\n", 160))...)
				if err := os.WriteFile(path, body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := index.ReindexFull(s.store, s.vaultRoot); err != nil {
				t.Fatal(err)
			}
		}
		items := make([]batchFetchItem, 0, notes*2)
		for i := 0; i < notes; i++ {
			id := fmt.Sprintf("n%d", i)
			if workload == "larger-nested-sections" {
				items = append(items, batchFetchItem{id, "parent"}, batchFetchItem{id, "child"})
			} else {
				items = append(items, batchFetchItem{id, ""})
			}
		}
		for _, callers := range []int{1, 4} {
			for _, workers := range []int{0, 1, 2, 4, 8} {
				mode := "batch"
				if workers == 0 {
					mode = "sequential-single"
				}
				// No parallel test mutates the process environment. Every round
				// joins all callers before the next configuration is installed.
				t.Setenv("MESH_FETCH_WORKERS", strconv.Itoa(max(1, workers)))
				var durations []float64
				var reference batchE2EMetrics
				for round := -warmups; round < rounds; round++ {
					elapsed, metrics, err := batchE2ERound(s, items, budget, workers != 0, callers)
					if err != nil {
						t.Fatal(err)
					}
					if round == -warmups {
						reference = metrics
					}
					if metrics.WorkersUsed != workers {
						t.Fatalf("workers_used=%d want configured %d", metrics.WorkersUsed, workers)
					}
					if !reflect.DeepEqual(reference, metrics) {
						t.Fatalf("unstable response accounting: %+v != %+v", reference, metrics)
					}
					if round >= 0 {
						durations = append(durations, elapsed.Seconds()*1000)
					}
				}
				sort.Float64s(durations)
				observations = append(observations, batchE2EObservation{
					Workload: workload, Mode: mode, Workers: workers, Callers: callers, Samples: rounds,
					MedianMS: durations[len(durations)/2], P95MS: durations[int(math.Ceil(.95*float64(len(durations))))-1],
					batchE2EMetrics: reference,
				})
			}
		}
	}
	report := map[string]any{
		"method":      "synthetic in-process HandleHTTP; request encoding through final JSON response; warm OS/SQLite caches after indexing and two warmups per arm; fixed sequential arm order; seven rounds; nearest-rank p95; no socket, auth middleware or model latency",
		"timing":      "wall time for all concurrent journeys to join; post-response coverage validation and token measurement excluded; request encoding, test recorder, response copying and lightweight RPC envelope decoding included",
		"tokens":      "per journey; batch budget applies to MCP result, not JSON-RPC framing; wire counts include framing; sequential singles have no shared budget; all measured arms must retain every requested fact with zero omissions",
		"limitations": "synthetic 8-note corpus is not a large-vault or cold-storage benchmark; per-request worker setting is not a global concurrency cap; seven-sample p95 is the maximum sample",
		"budget":      budget, "observations": observations,
	}
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("MESH_BATCH_E2E_JSON=%s\n", body)
}

func batchE2ERound(s *Server, items []batchFetchItem, budget int, batch bool, callers int) (time.Duration, batchE2EMetrics, error) {
	type journey struct {
		calls []batchE2ECall
		err   error
	}
	journeys := make([]journey, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for c := range journeys {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			<-start
			if batch {
				call, err := batchE2ERequest(s, context.Background(), "mesh_fetch_many", map[string]any{"items": items, "budget": budget})
				journeys[c] = journey{[]batchE2ECall{call}, err}
				return
			}
			for _, item := range items {
				call, err := batchE2ERequest(s, context.Background(), "mesh_fetch", item)
				if err != nil {
					journeys[c].err = err
					return
				}
				journeys[c].calls = append(journeys[c].calls, call)
			}
		}(c)
	}
	began := time.Now()
	close(start)
	wg.Wait()
	elapsed := time.Since(began)
	var reference batchE2EMetrics
	for c, journey := range journeys {
		if journey.err != nil {
			return elapsed, reference, journey.err
		}
		metrics := batchE2EMetrics{Requests: len(journey.calls)}
		for i, call := range journey.calls {
			metrics.ResultTokens += retrieve.EstimateTokens(string(call.Result))
			metrics.WireTokens += retrieve.EstimateTokens(string(call.Wire))
			metrics.WireBytes += len(call.Wire)
			if batch {
				result, err := batchE2ECheck(call, items, budget, nil)
				if err != nil {
					return elapsed, metrics, err
				}
				metrics.Omitted += len(result.Omitted)
				metrics.WorkersUsed = result.Workers
			} else {
				text, err := batchE2EText(call)
				if err != nil {
					return elapsed, metrics, err
				}
				if err := batchE2ERequired(text, items[i].Anchor); err != nil {
					return elapsed, metrics, err
				}
			}
		}
		if metrics.Omitted != 0 {
			return elapsed, metrics, fmt.Errorf("measurement lost %d inputs", metrics.Omitted)
		}
		if c == 0 {
			reference = metrics
		} else if !reflect.DeepEqual(reference, metrics) {
			return elapsed, metrics, fmt.Errorf("concurrent callers returned different accounting")
		}
	}
	return elapsed, reference, nil
}
