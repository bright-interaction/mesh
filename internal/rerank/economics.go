// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package rerank

import (
	"context"
	"time"
)

// CallStats is content-free accounting for one candidate-rerank request. Input
// and output use Mesh's bundled cl100k tokenizer; ProviderTokens holds the
// provider-reported total when the CLI exposes one.
type CallStats struct {
	Called           bool
	CacheHit         bool
	CircuitOpen      bool
	InputTokens      int
	OutputTokens     int
	ProviderTokens   int
	ProviderReported bool
	Duration         time.Duration
}

// AccountedTokens prefers the provider's total when one was reported. The
// prompt/output tokenizer estimate remains the portable fallback.
func (s CallStats) AccountedTokens() int {
	if s.ProviderReported {
		return s.ProviderTokens
	}
	return s.InputTokens + s.OutputTokens
}

// MeasuredCandidateReranker exposes per-call economics without global mutable
// "last call" state, which would attribute concurrent searches incorrectly.
type MeasuredCandidateReranker interface {
	RerankCandidatesMeasured(ctx context.Context, query string, candidates []Candidate) ([]Result, CallStats, error)
}

// PolicyReporter identifies whether a bounded subscription reranker should be
// applied to every eligible search or only to locally ambiguous searches.
type PolicyReporter interface {
	RerankPolicy() string
}
