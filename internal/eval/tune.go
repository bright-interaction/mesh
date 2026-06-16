package eval

import (
	"math"

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
func scoreWeights(r *retrieve.Retriever, cases []Case, w WeightSet) Score {
	s := Score{N: len(cases)}
	for _, c := range cases {
		want := map[string]bool{}
		for _, id := range c.Relevant {
			want["note:"+id] = true
		}
		cards, _ := r.Retrieve(c.Query, retrieve.Options{
			Limit:       surfaceK,
			NoRerank:    true,
			WeightFTS:   w.FTS,
			WeightGraph: w.Graph,
			WeightVec:   w.Vec,
		})
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
	return s
}

// simplexGrid enumerates weight triples on the grid of the given step that sum to
// 1.0. Relative weights are all that matter (min-max normalized signals, summed
// then sorted), so the simplex is the whole search space. When vectors is false
// the vector axis is held at 0.
func simplexGrid(step float64, vectors bool) []WeightSet {
	n := int(math.Round(1.0 / step))
	var out []WeightSet
	for i := 0; i <= n; i++ {
		if !vectors {
			out = append(out, WeightSet{float64(i) * step, float64(n-i) * step, 0})
			continue
		}
		for j := 0; i+j <= n; j++ {
			k := n - i - j
			out = append(out, WeightSet{float64(i) * step, float64(j) * step, float64(k) * step})
		}
	}
	return out
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
// enabled if vectors is true. The search never touches the rerank stage.
func TuneWeights(r *retrieve.Retriever, train, test []Case, step float64, vectors bool) TuneReport {
	grid := simplexGrid(step, vectors)
	def := defaultWeights(vectors)
	rep := TuneReport{Candidates: len(grid), Default: def}

	var best WeightSet
	var bestScore Score
	for gi, w := range grid {
		sc := scoreWeights(r, train, w)
		better := gi == 0 ||
			sc.Answer1 > bestScore.Answer1 ||
			(sc.Answer1 == bestScore.Answer1 && sc.Recall > bestScore.Recall)
		if better {
			best, bestScore = w, sc
		}
	}
	rep.Best, rep.BestTrain = best, bestScore
	rep.DefaultTrain = scoreWeights(r, train, def)
	rep.BestTest = scoreWeights(r, test, best)
	rep.DefaultTest = scoreWeights(r, test, def)
	return rep
}
