// Package eval is the Gate-1 measurement harness: it pits Mesh's budget-fitted
// retrieval against the "read the top-3 FTS notes" baseline on a labelled query
// set and reports tokens-to-answer and recall. Both arms are counted with the
// same tokenizer so the comparison is fair.
package eval

import (
	"os"
	"path/filepath"

	"github.com/brightinteraction/mesh/internal/index"
	"github.com/brightinteraction/mesh/internal/retrieve"
)

// Case is one labelled query: the relevant note ids the answer should come from.
type Case struct {
	Query    string   `json:"query"`
	Relevant []string `json:"relevant"`
}

// CaseResult is the per-query outcome for both arms.
type CaseResult struct {
	Query      string
	MeshTokens int
	BaseTokens int
	MeshHit    bool
	BaseHit    bool
}

// Report aggregates the run and renders the Gate-1 verdict.
type Report struct {
	Cases     []CaseResult
	N         int
	MeshHits  int
	BaseHits  int
	MeshAvg   float64
	BaseAvg   float64
	Pass      bool
}

// baselineTopK is how many FTS notes the baseline agent "reads" in full.
const baselineTopK = 3

// RunGate models two agent strategies per case:
//
//	baseline: FTS, then read the full body of the top-3 hits.
//	mesh:     fused search returns cheap cards, then read the full body of only
//	          the top card.
//
// Both succeed if a relevant note is surfaced. Gate 1 passes when Mesh matches
// or beats the baseline's recall at strictly fewer average tokens.
func RunGate(store *index.Store, r *retrieve.Retriever, vaultRoot string, cases []Case, budget int) Report {
	rep := Report{N: len(cases)}
	var meshSum, baseSum int

	for _, c := range cases {
		want := map[string]bool{}
		for _, id := range c.Relevant {
			want["note:"+id] = true
		}

		// Baseline: read the top-3 FTS bodies.
		fts, _ := store.Search(c.Query, baselineTopK)
		baseTok, baseHit := 0, false
		for _, h := range fts {
			baseTok += bodyTokens(vaultRoot, h.Path)
			if want[h.NodeID] {
				baseHit = true
			}
		}

		// Mesh: cards (cheap) + read the single top card's body.
		cards, _ := r.Retrieve(c.Query, retrieve.Options{Budget: budget})
		meshTok := retrieve.TotalTokens(cards)
		meshHit := false
		for _, card := range cards {
			if want[card.NodeID] {
				meshHit = true
			}
		}
		if len(cards) > 0 {
			meshTok += bodyTokens(vaultRoot, cards[0].Path)
		}

		rep.Cases = append(rep.Cases, CaseResult{c.Query, meshTok, baseTok, meshHit, baseHit})
		meshSum += meshTok
		baseSum += baseTok
		if meshHit {
			rep.MeshHits++
		}
		if baseHit {
			rep.BaseHits++
		}
	}

	if rep.N > 0 {
		rep.MeshAvg = float64(meshSum) / float64(rep.N)
		rep.BaseAvg = float64(baseSum) / float64(rep.N)
	}
	rep.Pass = rep.MeshHits >= rep.BaseHits && rep.MeshAvg < rep.BaseAvg
	return rep
}

func bodyTokens(vaultRoot, relPath string) int {
	if relPath == "" {
		return 0
	}
	data, err := os.ReadFile(filepath.Join(vaultRoot, relPath))
	if err != nil {
		return 0
	}
	return retrieve.EstimateTokens(string(data))
}
