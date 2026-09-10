// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/rerank"
)

func TestSubscriptionRouteSkipsOnlyExactOrClearlySeparatedFTS(t *testing.T) {
	cards := []Card{
		{NoteID: "mesh-routing", Title: "Mesh Routing", Path: "decisions/mesh-routing.md", Reason: "fts", Score: 1},
		{NoteID: "other", Title: "Other", Reason: "fts", Score: 0.2},
	}
	for _, query := range []string{"mesh-routing", "Mesh Routing", "decisions/mesh-routing.md"} {
		if call, route := subscriptionRoute(query, cards, "auto"); call || route != "local_exact" {
			t.Fatalf("query %q routed call=%t route=%q, want local_exact", query, call, route)
		}
	}
	if call, route := subscriptionRoute("mesh routing decision", []Card{
		{Title: "Mesh routing", Snippet: "decision confidence gate", Reason: "fts", Score: 1},
		{Title: "Other", Reason: "fts", Score: 0.4},
	}, "auto"); call || route != "local_confident" {
		t.Fatalf("strong FTS routed call=%t route=%q, want local_confident", call, route)
	}
	if call, route := subscriptionRoute("how should memory choose context", cards, "auto"); !call || route != "model" {
		t.Fatalf("ambiguous query routed call=%t route=%q, want model", call, route)
	}
	if call, route := subscriptionRoute("other", cards, "auto"); !call || route != "model" {
		t.Fatalf("exact lower card routed call=%t route=%q, want model", call, route)
	}
	if call, _ := subscriptionRoute("mesh-routing", cards, "always"); !call {
		t.Fatal("always policy skipped the reranker")
	}
}

type failingMeasuredReranker struct{ policy string }

func (f failingMeasuredReranker) Model() string        { return "subscription/test/tiny" }
func (f failingMeasuredReranker) CandidateLimit() int  { return 12 }
func (f failingMeasuredReranker) ResultLimit() int     { return 5 }
func (f failingMeasuredReranker) RerankPolicy() string { return f.policy }
func (f failingMeasuredReranker) Rerank(context.Context, string, []string) ([]rerank.Result, error) {
	return nil, errors.New("must use compact path")
}
func (f failingMeasuredReranker) RerankCandidates(context.Context, string, []rerank.Candidate) ([]rerank.Result, error) {
	return nil, errors.New("provider quota exhausted")
}
func (f failingMeasuredReranker) RerankCandidatesMeasured(context.Context, string, []rerank.Candidate) ([]rerank.Result, rerank.CallStats, error) {
	return nil, rerank.CallStats{Called: true, InputTokens: 77, Duration: 2 * time.Millisecond}, errors.New("provider quota exhausted")
}

func TestAutoSubscriptionFailureIsExplicitLocalFallback(t *testing.T) {
	r := buildVault(t)
	r.EnableRerank(failingMeasuredReranker{policy: "auto"})
	var economics Economics
	cards, err := r.Retrieve(context.Background(), "sqlite marketing", Options{Limit: 10, Economics: &economics})
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) == 0 || !economics.Fallback || economics.Route != "fallback" || !economics.RerankCalled || economics.RerankInput != 77 {
		t.Fatalf("fallback was not explicit and measured: cards=%d economics=%+v", len(cards), economics)
	}

	r.RecordEconomics(economics)
	if got, err := r.store.Metric("rerank:fallbacks"); err != nil || got != 1 {
		t.Fatalf("persisted fallback counter = %d, %v; want 1", got, err)
	}
}

func TestAlwaysSubscriptionFailureStillFailsLoudly(t *testing.T) {
	r := buildVault(t)
	r.EnableRerank(failingMeasuredReranker{policy: "always"})
	_, err := r.Retrieve(context.Background(), "sqlite storage", Options{Limit: 10})
	if !errors.Is(err, ErrRerankUnavailable) {
		t.Fatalf("always policy error = %v, want ErrRerankUnavailable", err)
	}
}

func TestFallbackReceiptIsOnlyPricedWhenItIsOnTheWire(t *testing.T) {
	normal := Economics{RerankConfigured: true, Route: "model", RerankInput: 10}
	if got := normal.ReceiptTokens(); got != 0 {
		t.Fatalf("normal route receipt cost = %d, want 0 because no receipt is sent", got)
	}
	fallback := Economics{RerankConfigured: true, Route: "fallback", Fallback: true, RerankInput: 10}
	if got := fallback.ReceiptTokens(); got <= 0 {
		t.Fatalf("fallback receipt cost = %d, want positive", got)
	}
	provider := fallback
	provider.ProviderReported = true
	provider.RerankProviderTokens = 3524
	if got := provider.ModelTokens(); got != 3524 {
		t.Fatalf("provider-reported usage lost: %d", got)
	}
}
