// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package eval

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/retrieve"
)

func TestSimplexGridSumsToOne(t *testing.T) {
	for _, vectors := range []bool{true, false} {
		grid := simplexGrid(0.1, vectors)
		if len(grid) == 0 {
			t.Fatalf("empty grid (vectors=%v)", vectors)
		}
		for _, w := range grid {
			sum := w.FTS + w.Graph + w.Vec
			if math.Abs(sum-1.0) > 1e-9 {
				t.Errorf("weights %v sum to %v, want 1.0", w, sum)
			}
			if !vectors && w.Vec != 0 {
				t.Errorf("vector axis should be 0 when vectors=false, got %v", w)
			}
		}
	}
	// 3-axis simplex on a step-0.5 grid: (i,j,k) with i+j+k=2 => 6 points.
	if n := len(simplexGrid(0.5, true)); n != 6 {
		t.Errorf("step-0.5 3D simplex should have 6 points, got %d", n)
	}
	// 2-axis: i in 0..2 => 3 points.
	if n := len(simplexGrid(0.5, false)); n != 3 {
		t.Errorf("step-0.5 2D simplex should have 3 points, got %d", n)
	}
}

func TestDefaultWeights(t *testing.T) {
	if d := defaultWeights(true); d != (WeightSet{0.5, 0.2, 0.3}) {
		t.Errorf("vector default wrong: %v", d)
	}
	if d := defaultWeights(false); d != (WeightSet{0.7, 0.3, 0}) {
		t.Errorf("lexical default wrong: %v", d)
	}
}

func tuneCases() []Case {
	return []Case{{Query: "storage", Relevant: []string{"storage"}}}
}

func TestTuneWeightsRetrievalFailureInvalidatesEveryPhase(t *testing.T) {
	// The step-0.5 lexical grid has three candidates, followed by three
	// baseline/held-out scoring passes. Each must fail closed, even if the
	// retriever returns usable-looking cards alongside its error.
	phases := []string{"candidate 1 train", "candidate 2 train", "candidate 3 train", "default train", "best test", "default test"}
	for failAt, phase := range phases {
		t.Run(phase, func(t *testing.T) {
			sentinel := errors.New("fixture retrieval failed")
			calls := 0
			fetch := func(context.Context, string, retrieve.Options) ([]retrieve.Card, error) {
				calls++
				cards := []retrieve.Card{{NodeID: "note:storage"}}
				if calls == failAt+1 {
					return cards, sentinel
				}
				return cards, nil
			}
			rep, err := tuneWeights(context.Background(), fetch, tuneCases(), tuneCases(), 0.5, false)
			if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), phase+" case 1") {
				t.Fatalf("missing phase or cause: %v", err)
			}
			if rep != (TuneReport{}) || calls != failAt+1 {
				t.Fatalf("failed run retained a verdict or continued: report=%+v calls=%d", rep, calls)
			}
		})
	}
}

func TestTuneWeightsPreservesContextAndSuccessScoring(t *testing.T) {
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "fixture")
	calls := 0
	fetch := func(got context.Context, query string, opt retrieve.Options) ([]retrieve.Card, error) {
		calls++
		if got != ctx || query != "storage" || !opt.NoRerank || opt.Limit != surfaceK {
			t.Fatalf("context or scoring contract changed: ctx=%v query=%q opt=%+v", got, query, opt)
		}
		return []retrieve.Card{{NodeID: "note:storage"}}, nil
	}
	rep, err := tuneWeights(ctx, fetch, tuneCases(), tuneCases(), 0.5, false)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 6 || rep.Candidates != 3 || rep.Best != (WeightSet{0, 1, 0}) || rep.Default != defaultWeights(false) {
		t.Fatalf("grid or tie-breaking changed: calls=%d report=%+v", calls, rep)
	}
	for _, score := range []Score{rep.BestTrain, rep.DefaultTrain, rep.BestTest, rep.DefaultTest} {
		if score != (Score{Answer1: 1, Recall: 1, N: 1}) {
			t.Fatalf("successful score changed: %+v", score)
		}
	}
}

func TestTuneWeightsCancellationInvalidatesRun(t *testing.T) {
	for _, cancelAt := range []int{0, 1, 6} {
		t.Run(fmt.Sprint(cancelAt), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if cancelAt == 0 {
				cancel()
			}
			calls := 0
			fetch := func(got context.Context, _ string, _ retrieve.Options) ([]retrieve.Card, error) {
				if got != ctx {
					t.Fatal("caller context was replaced")
				}
				calls++
				if calls == cancelAt {
					cancel()
				}
				return []retrieve.Card{{NodeID: "note:storage"}}, nil
			}
			rep, err := tuneWeights(ctx, fetch, tuneCases(), tuneCases(), 0.5, false)
			if !errors.Is(err, context.Canceled) || rep != (TuneReport{}) || calls != cancelAt {
				t.Fatalf("cancelled run continued or retained scores: %+v %v calls=%d", rep, err, calls)
			}
		})
	}
}

func TestTuneWeightsRejectsInvalidInputsBeforeRetrieval(t *testing.T) {
	for _, tc := range []struct {
		name        string
		train, test []Case
		step        float64
	}{
		{"no train", nil, tuneCases(), 0.5},
		{"no test", tuneCases(), nil, 0.5},
		{"blank query", []Case{{Query: " ", Relevant: []string{"storage"}}}, tuneCases(), 0.5},
		{"missing labels", tuneCases(), []Case{{Query: "storage"}}, 0.5},
		{"blank label", tuneCases(), []Case{{Query: "storage", Relevant: []string{"storage", " "}}}, 0.5},
		{"zero step", tuneCases(), tuneCases(), 0},
		{"negative step", tuneCases(), tuneCases(), -0.1},
		{"large step", tuneCases(), tuneCases(), 0.6},
		{"nan step", tuneCases(), tuneCases(), math.NaN()},
		{"infinite step", tuneCases(), tuneCases(), math.Inf(1)},
		{"overflowing grid", tuneCases(), tuneCases(), math.SmallestNonzeroFloat64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fetch := func(context.Context, string, retrieve.Options) ([]retrieve.Card, error) {
				t.Fatal("invalid inputs reached retrieval")
				return nil, nil
			}
			rep, err := tuneWeights(context.Background(), fetch, tc.train, tc.test, tc.step, false)
			if err == nil || rep != (TuneReport{}) {
				t.Fatalf("invalid input produced a report: %+v %v", rep, err)
			}
		})
	}
}
