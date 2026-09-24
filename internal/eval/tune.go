// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package eval

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/bright-interaction/mesh/internal/retrieve"
)

// WeightSet is one fusion-weight triple (FTS, graph-BM25, vector cosine).
type WeightSet struct{ FTS, Graph, Vec float64 }

// Score holds answer@1 and surfacing-recall counts for a weight set on a case set.
type Score struct{ Answer1, Recall, N int }

// TuneReport is the outcome of a weight search: the best set found on the train
// split and how it and the built-in default score on both train and test, so the
// reader can see at a glance whether the learned weights generalize or overfit.
type TuneReport struct {
	Candidates              int
	Default, Best           WeightSet
	DefaultTrain, BestTrain Score
	DefaultTest, BestTest   Score
}

// scoreWeights runs the fusion (rerank OFF, so the fused order is what is
// measured) with the given weights over cases and counts answer@1 (top card
// relevant) and surfacing recall (a relevant id anywhere in the candidate set).
// The retriever's query-embedding cache makes the repeated sweep cheap.
func scoreWeights(ctx context.Context, retrieveCards func(context.Context, string, retrieve.Options) ([]retrieve.Card, error), cases []Case, w WeightSet) (Score, error) {
	s := Score{N: len(cases)}
	for i, c := range cases {
		if err := ctx.Err(); err != nil {
			return Score{}, err
		}
		want := map[string]bool{}
		for _, id := range c.Relevant {
			want["note:"+id] = true
		}
		cards, err := retrieveCards(ctx, c.Query, retrieve.Options{
			Limit:       surfaceK,
			NoRerank:    true,
			WeightFTS:   w.FTS,
			WeightGraph: w.Graph,
			WeightVec:   w.Vec,
		})
		if err != nil {
			return Score{}, fmt.Errorf("case %d retrieval: %w", i+1, err)
		}
		if err := ctx.Err(); err != nil {
			return Score{}, err
		}
		if len(cards) > 0 && want[cards[0].NodeID] {
			s.Answer1++
		}
		for _, card := range cards {
			if want[card.NodeID] {
				s.Recall++
				break
			}
		}
	}
	return s, nil
}

// simplexGrid enumerates weight triples on the grid of the given step that sum to
// 1.0. Relative weights are all that matter (min-max normalized signals, summed
// then sorted), so the simplex is the whole search space. When vectors is false
// the vector axis is held at 0.
func simplexGrid(step float64, vectors bool) []WeightSet {
	grid, _ := simplexGridContext(context.Background(), step, vectors)
	return grid
}

func simplexGridContext(ctx context.Context, step float64, vectors bool) ([]WeightSet, error) {
	n := int(math.Round(1.0 / step))
	var out []WeightSet
	for i := 0; i <= n; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !vectors {
			out = append(out, WeightSet{float64(i) * step, float64(n-i) * step, 0})
			continue
		}
		for j := 0; i+j <= n; j++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			k := n - i - j
			out = append(out, WeightSet{float64(i) * step, float64(j) * step, float64(k) * step})
		}
	}
	return out, nil
}

// defaultWeights returns the built-in fusion weights for the comparison baseline.
func defaultWeights(vectors bool) WeightSet {
	if vectors {
		return WeightSet{0.5, 0.2, 0.3}
	}
	return WeightSet{0.7, 0.3, 0}
}

// TuneWeights grid-searches the weight simplex on train, maximizing answer@1
// (surfacing recall as the tiebreak), and reports how the winner and the built-in
// default score on both train and test. The retriever must already have vectors
// enabled if vectors is true. The search never touches the rerank stage. Failed
// retrieval or cancellation invalidates the entire run, never a zero-scored case.
func TuneWeights(ctx context.Context, r *retrieve.Retriever, train, test []Case, step float64, vectors bool) (TuneReport, error) {
	if r == nil {
		return TuneReport{}, fmt.Errorf("tune requires a retriever")
	}
	return tuneWeights(ctx, r.Retrieve, train, test, step, vectors)
}

// The function seam lets fixtures fail each scoring phase without model calls.
func tuneWeights(ctx context.Context, retrieveCards func(context.Context, string, retrieve.Options) ([]retrieve.Card, error), train, test []Case, step float64, vectors bool) (TuneReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return TuneReport{}, err
	}
	if math.IsNaN(step) || math.IsInf(step, 0) || step <= 0 || step > 0.5 || math.Round(1/step) >= float64(^uint(0)>>1) {
		return TuneReport{}, fmt.Errorf("tune step must be finite, greater than zero and at most 0.5, with a representable grid")
	}
	for _, split := range []struct {
		name  string
		cases []Case
	}{{"train", train}, {"test", test}} {
		if len(split.cases) == 0 {
			return TuneReport{}, fmt.Errorf("tune requires non-empty %s cases", split.name)
		}
		for i, c := range split.cases {
			if strings.TrimSpace(c.Query) == "" || len(c.Relevant) == 0 {
				return TuneReport{}, fmt.Errorf("tune %s case %d: query and relevant labels are required", split.name, i+1)
			}
			for _, id := range c.Relevant {
				if strings.TrimSpace(id) == "" {
					return TuneReport{}, fmt.Errorf("tune %s case %d: relevant labels must not be empty", split.name, i+1)
				}
			}
		}
	}
	grid, err := simplexGridContext(ctx, step, vectors)
	if err != nil {
		return TuneReport{}, err
	}
	def := defaultWeights(vectors)
	rep := TuneReport{Candidates: len(grid), Default: def}

	var best WeightSet
	var bestScore Score
	for gi, w := range grid {
		sc, err := scoreWeights(ctx, retrieveCards, train, w)
		if err != nil {
			return TuneReport{}, fmt.Errorf("tune candidate %d train %w", gi+1, err)
		}
		better := gi == 0 ||
			sc.Answer1 > bestScore.Answer1 ||
			(sc.Answer1 == bestScore.Answer1 && sc.Recall > bestScore.Recall)
		if better {
			best, bestScore = w, sc
		}
	}
	rep.Best, rep.BestTrain = best, bestScore
	if rep.DefaultTrain, err = scoreWeights(ctx, retrieveCards, train, def); err != nil {
		return TuneReport{}, fmt.Errorf("tune default train %w", err)
	}
	if rep.BestTest, err = scoreWeights(ctx, retrieveCards, test, best); err != nil {
		return TuneReport{}, fmt.Errorf("tune best test %w", err)
	}
	if rep.DefaultTest, err = scoreWeights(ctx, retrieveCards, test, def); err != nil {
		return TuneReport{}, fmt.Errorf("tune default test %w", err)
	}
	return rep, nil
}
