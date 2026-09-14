// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/retrieve"
)

// These are scripted client decisions, not production routing or evidence that
// a model follows the guidance. Accounting covers serialized tool responses;
// request, initialization, model input/output and network costs are separate.
type retrievalJourney struct {
	Calls           int           `json:"calls"`
	ResultTokens    int           `json:"mcp_result_tokens"`
	WireTokens      int           `json:"jsonrpc_response_tokens"`
	Complete        bool          `json:"complete"`
	Elapsed         time.Duration `json:"-"`
	texts           []string
	covered         map[int]bool
	followup        []int // Original indices represented by the one follow-up input.
	followupOmitted []int // Indices in the follow-up request, not the original.
	statuses        []string
}

func (j *retrievalJourney) call(t *testing.T, s *Server, tool string, args any) batchE2ECall {
	t.Helper()
	began := time.Now()
	call, err := batchE2ERequest(s, context.Background(), tool, args)
	j.Elapsed += time.Since(began)
	if err != nil {
		t.Fatal(err)
	}
	if call.Error != nil {
		t.Fatalf("%s: %+v", tool, call.Error)
	}
	j.Calls++
	j.ResultTokens += retrieve.EstimateTokens(string(call.Result))
	j.WireTokens += retrieve.EstimateTokens(string(call.Wire))
	return call
}

func journeyText(t *testing.T, call batchE2ECall) string {
	t.Helper()
	text, err := batchE2EText(call)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func journeyBatch(t *testing.T, j *retrievalJourney, s *Server, items []batchFetchItem, budget int) batchFetchResponse {
	t.Helper()
	call := j.call(t, s, "mesh_fetch_many", map[string]any{"items": items, "budget": budget})
	var out batchFetchResponse
	if err := json.Unmarshal([]byte(journeyText(t, call)), &out); err != nil {
		t.Fatal(err)
	}
	if actual := retrieve.EstimateTokens(string(call.Result)); actual > out.Tokens || out.Tokens > budget {
		t.Fatalf("response=%d receipt=%d budget=%d", actual, out.Tokens, budget)
	}
	return out
}

// Only budget omissions qualify for a follow-up. A single highest-priority
// omitted section (deduplicating identical inputs) is requested once, bounded
// by the remaining MCP-result allowance. Unavailable/too_large never fall back
// to whole-note reads; an omitted whole note is not blindly retried either.
func journeyBounded(t *testing.T, s *Server, items []batchFetchItem, priority []int, firstBudget, total int, prior ...retrievalJourney) retrievalJourney {
	t.Helper()
	j := retrievalJourney{covered: map[int]bool{}}
	for _, p := range prior { // Search/resource responses are not free setup.
		j.Calls += p.Calls
		j.ResultTokens += p.ResultTokens
		j.WireTokens += p.WireTokens
		j.Elapsed += p.Elapsed
	}
	previousCalls := j.Calls
	if total-j.ResultTokens < 256 {
		return j
	}
	first := journeyBatch(t, &j, s, items, min(firstBudget, total-j.ResultTokens, 32000))
	omitted := map[int]bool{}
	seen := map[int]bool{}
	for _, r := range first.Results {
		j.statuses = append(j.statuses, r.Status)
		for _, i := range r.Indices {
			if i < 0 || i >= len(items) || seen[i] || items[i].ID != r.ID {
				t.Fatalf("invalid result mapping: %+v", r)
			}
			seen[i] = true
			if r.Status == "ok" {
				j.covered[i] = true
				if err := batchE2ERequired(r.Text, items[i].Anchor); err != nil {
					t.Fatal(err)
				}
			}
		}
		j.texts = append(j.texts, r.Text)
	}
	for _, i := range first.Omitted {
		if i < 0 || i >= len(items) || seen[i] {
			t.Fatalf("invalid omission mapping: %v", first.Omitted)
		}
		seen[i], omitted[i] = true, true
	}
	if len(seen) != len(items) {
		t.Fatal("response silently lost an input")
	}
	remaining := total - j.ResultTokens
	if remaining >= 256 {
		for _, i := range priority {
			if !omitted[i] || items[i].Anchor == "" {
				continue
			}
			// Follow-up indices restart at zero; never interpret them as
			// indices into the original request without this explicit map.
			for original, item := range items {
				if omitted[original] && item == items[i] {
					j.followup = append(j.followup, original)
				}
			}
			next := journeyBatch(t, &j, s, []batchFetchItem{items[i]}, min(32000, remaining))
			j.followupOmitted = append([]int(nil), next.Omitted...)
			if len(next.Results) == 1 && next.Results[0].Status == "ok" {
				r := next.Results[0]
				if r.ID != items[i].ID || !reflect.DeepEqual(r.Indices, []int{0}) || len(next.Omitted) != 0 {
					t.Fatalf("invalid follow-up mapping: %+v", next)
				}
				if err := batchE2ERequired(r.Text, items[i].Anchor); err != nil {
					t.Fatal(err)
				}
				for _, original := range j.followup {
					j.covered[original] = true
				}
				j.texts = append(j.texts, r.Text)
			}
			break // One opportunity, even when the follow-up is omitted.
		}
	}
	j.Complete = len(j.covered) == len(items)
	if j.Calls > previousCalls+2 || j.ResultTokens > total {
		t.Fatalf("unbounded journey: %+v allowance=%d", j, total)
	}
	return j
}

func journeySections(t *testing.T, s *Server, items []batchFetchItem, batch bool) retrievalJourney {
	t.Helper()
	j := retrievalJourney{Complete: true}
	if batch {
		call := j.call(t, s, "mesh_fetch_many", map[string]any{"items": items, "budget": 8000})
		result, err := batchE2ECheck(call, items, 8000, nil)
		if err != nil || len(result.Omitted) != 0 {
			t.Fatalf("batch coverage: %+v, %v", result, err)
		}
		for _, r := range result.Results {
			j.texts = append(j.texts, r.Text)
		}
		return j
	}
	for _, item := range items {
		text := journeyText(t, j.call(t, s, "mesh_fetch", item))
		if err := batchE2ERequired(text, item.Anchor); err != nil {
			t.Fatal(err)
		}
		j.texts = append(j.texts, text)
	}
	return j
}

func journeyCards(t *testing.T, s *Server) retrievalJourney {
	j := retrievalJourney{}
	text := journeyText(t, j.call(t, s, "mesh_search", map[string]any{"query": "sqlite storage", "budget": 1200, "limit": 1}))
	var out struct {
		Cards []struct {
			NoteID  string `json:"NoteID"`
			Snippet string `json:"Snippet"`
		} `json:"cards"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	// The scripted question is only which storage implementation the decision
	// names. This is not permission to execute a task without its caveats.
	j.Complete = len(out.Cards) == 1 && out.Cards[0].NoteID == "sqlite" && strings.Contains(out.Cards[0].Snippet, "modernc") && strings.Contains(out.Cards[0].Snippet, "sqlite")
	if !j.Complete {
		t.Fatalf("card did not answer the fixture question: %s", text)
	}
	return j // No fetch after a sufficient card.
}

func TestRetrievalJourneyDecisions(t *testing.T) {
	t.Setenv("MESH_FETCH_WORKERS", "2")
	if j := journeyCards(t, newTestServer(t)); j.Calls != 1 {
		t.Fatal("sufficient card caused an unnecessary fetch")
	}
	s := batchFixture(t, 3)
	batchE2EReady(s)
	if j := journeySections(t, s, []batchFetchItem{{"n0", "child"}}, false); j.Calls != 1 {
		t.Fatal("one missing section needs one single fetch")
	}
	items := []batchFetchItem{{"n0", "parent"}, {"n0", "child"}, {"n1", "parent"}, {"n1", "child"}}
	singles, batch := journeySections(t, s, items, false), journeySections(t, s, items, true)
	if singles.Calls != 4 || batch.Calls != 1 || batch.WireTokens >= singles.WireTokens {
		t.Fatalf("overlap fixture should reduce calls and response tokens: singles=%+v batch=%+v", singles, batch)
	}
	for _, text := range batch.texts {
		if strings.Count(text, "child answer") != 1 || strings.Count(text, sectionContextNotice) != 1 {
			t.Fatal("batch repeated overlapping content or safety header")
		}
	}
	budgetItems := []batchFetchItem{{"n0", "parent"}, {"n1", "child"}, {"n1", "child"}}
	j := journeyBounded(t, s, budgetItems, []int{2, 1, 0}, 256, 1000)
	if !j.Complete || j.Calls != 2 || !reflect.DeepEqual(j.followup, []int{1, 2}) {
		t.Fatalf("prioritized omission remapping failed: %+v", j)
	}
	for _, total := range []int{255, 256} {
		j = journeyBounded(t, s, budgetItems, []int{2, 1, 0}, 256, total)
		if j.Complete || j.Calls > 1 {
			t.Fatalf("insufficient allowance must stop incomplete: %+v", j)
		}
	}
	setup := journeySetup(t, s, "resources/read", map[string]any{"uri": "mesh://contract"})
	j = journeyBounded(t, s, budgetItems, []int{2, 1, 0}, 256, setup.ResultTokens+255, setup)
	if j.Complete || j.Calls != setup.Calls || j.ResultTokens != setup.ResultTokens {
		t.Fatalf("resource response ignored in total allowance: %+v", j)
	}
}

func TestRetrievalJourneyFailureTerminates(t *testing.T) {
	s := batchFixture(t, 1)
	batchE2EReady(s)
	for _, item := range []batchFetchItem{{"missing", "child"}, {"n0", "missing-heading"}} {
		j := journeyBounded(t, s, []batchFetchItem{item}, []int{0}, 8000, 16000)
		if j.Complete || j.Calls != 1 || len(j.followup) != 0 || !reflect.DeepEqual(j.statuses, []string{"unavailable"}) {
			t.Fatalf("unavailable triggered fallback: %+v", j)
		}
	}
	// The first omission is retried once, but still does not fit. No third call.
	large := "---\nid: n0\n---\n# Note\n## Child\n" + strings.Repeat("child answer reference detail\n", 1000)
	if err := os.WriteFile(filepath.Join(s.vaultRoot, "n0.md"), []byte(large), 0600); err != nil {
		t.Fatal(err)
	}
	j := journeyBounded(t, s, []batchFetchItem{{"n0", "child"}}, []int{0}, 256, 600)
	if j.Complete || j.Calls != 2 || !reflect.DeepEqual(j.followup, []int{0}) || !reflect.DeepEqual(j.followupOmitted, []int{0}) {
		t.Fatalf("repeated omission did not stop after one retry: %+v", j)
	}
	// Change only fixture data after indexing, preserving the indexed note ID.
	if err := os.WriteFile(filepath.Join(s.vaultRoot, "n0.md"), []byte(strings.Repeat("x", batchFetchFileBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	j = journeyBounded(t, s, []batchFetchItem{{"n0", "child"}}, []int{0}, 8000, 16000)
	if j.Complete || j.Calls != 1 || len(j.followup) != 0 || !reflect.DeepEqual(j.statuses, []string{"too_large"}) {
		t.Fatalf("too_large triggered fallback: %+v", j)
	}
}

// Optional setup is reported separately from the six decision scenarios. If a
// client loads it during a bounded journey, pass its accounting as prior costs.
func journeySetup(t *testing.T, s *Server, method string, params any) retrievalJourney {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.HandleHTTP(rec, httptest.NewRequest("POST", "/mcp", bytes.NewReader(body)))
	var call batchE2ECall
	if err := json.Unmarshal(rec.Body.Bytes(), &call); err != nil || call.Error != nil || rec.Code != 200 {
		t.Fatalf("setup failed: %v %+v HTTP %d", err, call.Error, rec.Code)
	}
	return retrievalJourney{Calls: 1, ResultTokens: retrieve.EstimateTokens(string(call.Result)), WireTokens: retrieve.EstimateTokens(rec.Body.String()), Complete: true}
}

func TestRetrievalJourneyEvaluation(t *testing.T) {
	if os.Getenv("MESH_RETRIEVAL_JOURNEY_EVAL") != "1" {
		t.Skip("set MESH_RETRIEVAL_JOURNEY_EVAL=1 for scripted warm response measurements")
	}
	t.Setenv("MESH_FETCH_WORKERS", "2")
	s, search := batchFixture(t, 2), newTestServer(t)
	batchE2EReady(s)
	items := []batchFetchItem{{"n0", "parent"}, {"n0", "child"}, {"n1", "parent"}, {"n1", "child"}}
	var rows []map[string]any
	for _, mode := range []string{"cards-sufficient", "single-section", "overlap-sequential", "overlap-batch", "budget-followup", "budget-stop"} {
		var reference retrievalJourney
		var timings []float64
		for round := -2; round < 7; round++ {
			var j retrievalJourney
			switch mode {
			case "cards-sufficient":
				j = journeyCards(t, search)
			case "single-section":
				j = journeySections(t, s, items[1:2], false)
			case "overlap-sequential", "overlap-batch":
				j = journeySections(t, s, items, mode == "overlap-batch")
			default:
				total := 1000
				if mode == "budget-stop" {
					total = 256
				}
				j = journeyBounded(t, s, []batchFetchItem{{"n0", "parent"}, {"n1", "child"}, {"n1", "child"}}, []int{2, 1, 0}, 256, total)
			}
			if round >= 0 {
				timings = append(timings, j.Elapsed.Seconds()*1000)
			}
			reference = j
		}
		sort.Float64s(timings)
		rows = append(rows, map[string]any{"mode": mode, "response": reference, "handler_median_ms": timings[3], "handler_max_ms": timings[6]})
	}
	report, err := json.Marshal(map[string]any{
		"optional_contract_response": journeySetup(t, s, "resources/read", map[string]any{"uri": "mesh://contract"}),
		"initialize_response":        journeySetup(t, s, "initialize", map[string]any{}),
		"method":                     "scripted decisions, in-process HandleHTTP; two warmups, seven rounds; warm synthetic fixtures; timings exclude decision logic and token validation; no timing correctness gate",
		"limitations":                "not model adherence or total model-token savings; no requests, initialization, sockets, authentication middleware, or model costs in response-token totals; each scenario states its own required evidence; incomplete is not a successful answer",
		"allowance":                  "budget-followup and budget-stop share a total serialized MCP-result response allowance across both calls; JSON-RPC framing measured separately, not covered by the tool budget",
		"observations":               rows,
	})
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("MESH_RETRIEVAL_JOURNEY_JSON=%s\n", report)
}
