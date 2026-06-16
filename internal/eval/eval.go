// Package eval is the Gate-1 measurement harness. An adversarial review of the
// first version found two fatal defects: it compared a 1-body Mesh arm against a
// 3-body baseline (so the "saving" was mostly body-count, not fusion), and it
// mixed two recall definitions (candidates-surfaced for Mesh vs bodies-read for
// the baseline). This version fixes both: three arms with matched costs, and
// surfacing-recall (at equal candidate K) reported separately from answer@1 (the
// single body each arm actually reads). Both arms use the same tokenizer.
package eval

import (
	"os"
	"path/filepath"
	"sort"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/retrieve"
)

// Case is one labelled query: the relevant note ids the answer should come from.
type Case struct {
	Query    string   `json:"query"`
	Relevant []string `json:"relevant"`
}

// surfaceK is the equal-size candidate pool for the surfacing-recall metric.
const surfaceK = 20

// CaseResult holds the per-query outcome across the three arms.
type CaseResult struct {
	Query string
	// Surfacing recall at equal K: does a relevant id appear in the candidate set?
	MeshSurfaced bool
	FTSSurfaced  bool
	// Answer@1: is the single body the arm reads (top card / top FTS hit) relevant?
	MeshAnswer1 bool
	FTSAnswer1  bool
	// Tokens for the body(ies) each arm actually reads, plus Mesh's cards.
	MeshTokens    int // cards + top-1 body (what Mesh costs)
	FTSTop1Tokens int // 1 body (matched single-read baseline)
	FTSTop3Tokens int // 3 bodies (naive baseline)
}

// Report aggregates the run.
type Report struct {
	Cases []CaseResult
	N     int

	MeshSurfaced, FTSSurfaced int // surfacing recall (equal K)
	MeshAnswer1, FTSAnswer1   int // answer@1

	MeshMean, FTSTop1Mean, FTSTop3Mean       float64
	MeshMedian, FTSTop1Median, FTSTop3Median float64

	// The three defensible sub-claims.
	SurfacingWin bool // Mesh surfaces relevant at equal K at least as often
	AnswerWin    bool // Mesh's single read is relevant at least as often
	NaiveCostWin bool // Mesh median tokens < naive read-top-3 median
	Pass         bool // all three hold
}

func RunGate(store *index.Store, r *retrieve.Retriever, vaultRoot string, cases []Case, budget int) Report {
	rep := Report{N: len(cases)}
	var mesh, fts1, fts3 []int

	for _, c := range cases {
		want := map[string]bool{}
		for _, id := range c.Relevant {
			want["note:"+id] = true
		}

		fts, _ := store.Search(c.Query, surfaceK)
		cards, _ := r.Retrieve(c.Query, retrieve.Options{Budget: budget})

		cr := CaseResult{Query: c.Query}

		// Surfacing recall at equal K.
		for i, h := range fts {
			if i >= surfaceK {
				break
			}
			if want[h.NodeID] {
				cr.FTSSurfaced = true
			}
		}
		for _, card := range cards {
			if want[card.NodeID] {
				cr.MeshSurfaced = true
			}
		}

		// Answer@1: the one body each arm reads.
		if len(fts) > 0 {
			cr.FTSAnswer1 = want[fts[0].NodeID]
			cr.FTSTop1Tokens = bodyTokens(vaultRoot, fts[0].Path)
		}
		for i := 0; i < 3 && i < len(fts); i++ {
			cr.FTSTop3Tokens += bodyTokens(vaultRoot, fts[i].Path)
		}
		if len(cards) > 0 {
			cr.MeshAnswer1 = want[cards[0].NodeID]
			cr.MeshTokens = retrieve.TotalTokens(cards) + bodyTokens(vaultRoot, cards[0].Path)
		}

		rep.Cases = append(rep.Cases, cr)
		if cr.MeshSurfaced {
			rep.MeshSurfaced++
		}
		if cr.FTSSurfaced {
			rep.FTSSurfaced++
		}
		if cr.MeshAnswer1 {
			rep.MeshAnswer1++
		}
		if cr.FTSAnswer1 {
			rep.FTSAnswer1++
		}
		mesh = append(mesh, cr.MeshTokens)
		fts1 = append(fts1, cr.FTSTop1Tokens)
		fts3 = append(fts3, cr.FTSTop3Tokens)
	}

	rep.MeshMean, rep.MeshMedian = mean(mesh), median(mesh)
	rep.FTSTop1Mean, rep.FTSTop1Median = mean(fts1), median(fts1)
	rep.FTSTop3Mean, rep.FTSTop3Median = mean(fts3), median(fts3)

	rep.SurfacingWin = rep.MeshSurfaced >= rep.FTSSurfaced
	rep.AnswerWin = rep.MeshAnswer1 >= rep.FTSAnswer1
	rep.NaiveCostWin = rep.MeshMedian < rep.FTSTop3Median
	rep.Pass = rep.SurfacingWin && rep.AnswerWin && rep.NaiveCostWin
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

func mean(xs []int) float64 {
	if len(xs) == 0 {
		return 0
	}
	sum := 0
	for _, x := range xs {
		sum += x
	}
	return float64(sum) / float64(len(xs))
}

func median(xs []int) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int(nil), xs...)
	sort.Ints(s)
	n := len(s)
	if n%2 == 1 {
		return float64(s[n/2])
	}
	return float64(s[n/2-1]+s[n/2]) / 2
}
