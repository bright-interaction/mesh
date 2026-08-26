// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/bright-interaction/mesh/internal/rerank"
)

type malformedIndexReranker struct{ kind string }

func (m malformedIndexReranker) Model() string { return "malformed-index" }

func (m malformedIndexReranker) Rerank(_ context.Context, _ string, docs []string) ([]rerank.Result, error) {
	out := make([]rerank.Result, len(docs))
	for i := range out {
		out[i] = rerank.Result{Index: i, Score: float64(i + 1)}
	}
	switch m.kind {
	case "negative":
		out[len(out)-1].Index = -1
	case "past-end":
		out[len(out)-1].Index = len(out)
	case "duplicate":
		out[len(out)-1].Index = 0
	case "nan-score":
		out[len(out)-1].Score = math.NaN()
	}
	return out, nil
}

func TestMalformedRerankerResultsFailWithoutPanicking(t *testing.T) {
	for _, kind := range []string{"negative", "past-end", "duplicate", "nan-score"} {
		t.Run(kind, func(t *testing.T) {
			r := buildVault(t)
			r.EnableRerank(malformedIndexReranker{kind: kind})
			deferred := false
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						deferred = true
						t.Errorf("malformed reranker response panicked: %v", recovered)
					}
				}()
				_, err := r.Retrieve(context.Background(), "sqlite storage", Options{Limit: 10})
				if !errors.Is(err, ErrRerankUnavailable) {
					t.Errorf("malformed reranker response must fail with ErrRerankUnavailable, got %v", err)
				}
			}()
			if deferred {
				return
			}
		})
	}
}

func TestRetrieveRejectsNonFiniteWeights(t *testing.T) {
	tests := []struct {
		name string
		opt  Options
	}{
		{name: "fts nan", opt: Options{WeightFTS: math.NaN()}},
		{name: "graph positive infinity", opt: Options{WeightGraph: math.Inf(1)}},
		{name: "vector negative infinity", opt: Options{WeightVec: math.Inf(-1)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := buildVault(t)
			tc.opt.NoRerank = true
			_, err := r.Retrieve(context.Background(), "sqlite storage", tc.opt)
			if !errors.Is(err, ErrInvalidWeights) {
				t.Errorf("non-finite weights must be rejected, got %v", err)
			}
		})
	}

	t.Run("configured default", func(t *testing.T) {
		r := buildVault(t)
		r.SetWeights(math.NaN(), 0.3, 0)
		_, err := r.Retrieve(context.Background(), "sqlite storage", Options{NoRerank: true})
		if !errors.Is(err, ErrInvalidWeights) {
			t.Errorf("non-finite configured weight must be rejected, got %v", err)
		}
	})
}

type staticEmbedder struct{}

func (staticEmbedder) Model() string { return "static-two" }
func (staticEmbedder) Dim() int      { return 2 }
func (staticEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

type failingEmbedder struct{ err error }

func (f failingEmbedder) Model() string { return "static-two" }
func (f failingEmbedder) Dim() int      { return 2 }
func (f failingEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return nil, f.err
}

func TestConfiguredEmbeddingFailureIsReturned(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{{"endpoint failure", errors.New("embed boom")}, {"cancellation", context.Canceled}} {
		t.Run(tc.name, func(t *testing.T) {
			r := buildVault(t)
			if !r.EnableVectors(failingEmbedder{err: tc.err}, "static-two", 2,
				map[string][][]float32{"note:a": {{1, 0}}}) {
				t.Fatal("EnableVectors failed")
			}
			_, err := r.Retrieve(context.Background(), "storage", Options{
				WeightVec: 1, NoRerank: true,
			})
			if !errors.Is(err, ErrEmbeddingUnavailable) || !errors.Is(err, tc.err) {
				t.Fatalf("configured embed failure = %v, want ErrEmbeddingUnavailable wrapping %v", err, tc.err)
			}
		})
	}
}

func TestZeroWeightLanesDoNotIntroduceCandidates(t *testing.T) {
	r := buildVaultFrom(t, []noteSrc{
		{"lexical.md", "---\nid: lexical\ntype: note\nwhen: 2026-01-01\n---\n# Needle reference\nneedle needle needle\n"},
		{"semantic.md", "---\nid: semantic\ntype: note\nwhen: 2026-01-01\n---\n# Orange handbook\ncompletely different prose\n"},
	})
	if !r.EnableVectors(staticEmbedder{}, "static-two", 2, map[string][][]float32{
		"note:semantic": {{1, 0}},
	}) {
		t.Fatal("EnableVectors failed")
	}

	cards, err := r.Retrieve(context.Background(), "needle", Options{
		Limit: 10, WeightFTS: 0, WeightGraph: 0, WeightVec: 1, NoRerank: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cardIDs(cards), []string{"note:semantic"}; !equalStrings(got, want) {
		t.Errorf("vector-only retrieval got %v, want %v; zero-weight lexical/graph lanes introduced candidates", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestPackToBudgetIsAHardSerializedPayloadLimit(t *testing.T) {
	cards := productionShapedCards(12)
	max := wireTokens(cards)
	for budget := 1; budget <= max; budget++ {
		packed := packToBudget(cards, budget)
		if got := wireTokens(packed); got > budget {
			t.Fatalf("budget %d emitted a %d-token JSON array (%d cards)", budget, got, len(packed))
		}
	}
}

func TestTotalTokensCountsTheSerializedArray(t *testing.T) {
	cards := productionShapedCards(5)
	if got, want := TotalTokens(cards), wireTokens(cards); got != want {
		t.Errorf("TotalTokens = %d, want exact serialized-array cost %d", got, want)
	}
}
