// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package eval is the Gate-1 measurement harness. An adversarial review of the
// first version found two fatal defects: it compared a 1-body Mesh arm against a
// 3-body baseline (so the "saving" was mostly body-count, not fusion), and it
// mixed two recall definitions (candidates-surfaced for Mesh vs bodies-read for
// the baseline). This version fixes both: three arms with matched costs, and
// surfacing-recall (at equal candidate K) reported separately from answer@1 (the
// single body each arm actually reads). Both arms use the same tokenizer.
package eval

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

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
	Query                                                     string
	Errors                                                    []string
	FTSTop1Millis, FTSTop3Millis, LocalMeshMillis, MeshMillis float64
	// Surfacing recall at equal K: does a relevant id appear in the candidate set?
	MeshSurfaced bool
	FTSSurfaced  bool
	// Answer@1: is the single body the arm reads (top card / top FTS hit) relevant?
	MeshAnswer1 bool
	FTSAnswer1  bool
	// LocalMesh is the identical fused retrieval with rerank disabled. It is the
	// honest baseline for deciding whether a second subscription-model call pays.
	LocalMeshSurfaced  bool
	LocalMeshAnswer1   bool
	RerankTop5Surfaced bool
	// Tokens for the body(ies) each arm actually reads, plus Mesh's cards.
	MeshTokens      int // cards + top-1 body (what Mesh costs)
	FTSTop1Tokens   int // 1 body (matched single-read baseline)
	FTSTop3Tokens   int // 3 bodies (naive baseline)
	LocalMeshTokens int // local cards + top-1 body, no second model
	RerankTokens    int // estimated prompt + output tokens actually sent on a cache miss
	CombinedTokens  int // reranked cards + top-1 body + rerank tokens
	RerankRoute     string
}

// Report aggregates the run.
type Report struct {
	LatencyMethod                                                 string
	Cases                                                         []CaseResult
	N                                                             int
	Valid                                                         bool
	Errors                                                        []string
	FTSTop1Latency, FTSTop3Latency, LocalMeshLatency, MeshLatency LatencySummary

	MeshSurfaced, FTSSurfaced                               int // surfacing recall (equal K)
	MeshAnswer1, FTSAnswer1                                 int // answer@1
	LocalMeshSurfaced, LocalMeshAnswer1, RerankTop5Surfaced int

	MeshMean, FTSTop1Mean, FTSTop3Mean            float64
	MeshMedian, FTSTop1Median, FTSTop3Median      float64
	LocalMeshMean, LocalMeshMedian                float64
	CombinedMean, CombinedMedian                  float64
	RerankCalls, RerankCacheHits, RerankFallbacks int
	RerankInputTokens, RerankOutputTokens         int
	RerankTokens, ProviderReportedCalls           int
	RerankEvaluated                               bool
	RerankQualityWin, RerankCostWin, RerankPass   bool

	// The three defensible sub-claims.
	SurfacingWin bool // Mesh surfaces relevant at equal K at least as often
	AnswerWin    bool // Mesh's single read is relevant at least as often
	NaiveCostWin bool // Mesh median tokens < naive read-top-3 median
	Pass         bool // all three hold
}

// Latencies include retrieval, packing/model work and the bodies each arm reads.
// Arms run sequentially (FTS, local, configured); caches are not reset. These
// are observed warm-process timings, not cold-start or production SLO claims.
type LatencySummary struct {
	Samples                 int
	MedianMillis, P95Millis float64
}

func RunGate(store *index.Store, r *retrieve.Retriever, vaultRoot string, cases []Case, budget int) Report {
	return RunGateContext(context.Background(), store, r, vaultRoot, cases, budget)
}

func RunGateContext(ctx context.Context, store *index.Store, r *retrieve.Retriever, vaultRoot string, cases []Case, budget int) Report {
	rep := Report{
		N:             len(cases),
		LatencyMethod: "retrieval + packing/model work + body reads; sequential FTS/local/configured; caches not reset; excludes setup; successful cases only; nearest-rank p95",
	}
	var mesh, fts1, fts3, localMesh, combined []int
	var meshMS, localMS, fts1MS, fts3MS []float64
	if len(cases) == 0 {
		rep.Errors = append(rep.Errors, "no labelled cases")
	}

	for caseIndex, c := range cases {
		if err := ctx.Err(); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("evaluation interrupted: %v", err))
			break
		}
		labelled := len(c.Relevant) > 0
		for _, id := range c.Relevant {
			if strings.TrimSpace(id) == "" {
				labelled = false
			}
		}
		if strings.TrimSpace(c.Query) == "" || !labelled {
			rep.Errors = append(rep.Errors, fmt.Sprintf("case %d: query and relevant labels are required", caseIndex+1))
			continue
		}
		want := map[string]bool{}
		for _, id := range c.Relevant {
			want["note:"+id] = true
		}

		cr := CaseResult{Query: c.Query}
		record := func(arm string, err error) {
			if err != nil {
				cr.Errors = append(cr.Errors, fmt.Sprintf("%s: %v", arm, err))
			}
		}
		readBody := func(arm, relPath string) int {
			tokens, err := bodyTokens(vaultRoot, relPath)
			record(arm+" body", err)
			return tokens
		}
		started := time.Now()
		fts, err := store.Search(ctx, c.Query, surfaceK)
		record("fts retrieval", err)
		if len(fts) > 0 {
			cr.FTSTop1Tokens = readBody("fts", fts[0].Path)
		}
		cr.FTSTop1Millis = elapsedMillis(started)
		cr.FTSTop3Tokens = cr.FTSTop1Tokens
		for i := 1; i < 3 && i < len(fts); i++ {
			cr.FTSTop3Tokens += readBody("fts", fts[i].Path)
		}
		cr.FTSTop3Millis = elapsedMillis(started)
		started = time.Now()
		localCards, err := r.Retrieve(ctx, c.Query, retrieve.Options{Budget: budget, NoRerank: true})
		record("local retrieval", err)
		if len(localCards) > 0 {
			cr.LocalMeshTokens = retrieve.TotalTokens(localCards) + readBody("local", localCards[0].Path)
		}
		cr.LocalMeshMillis = elapsedMillis(started)
		var economics retrieve.Economics
		started = time.Now()
		cards, err := r.Retrieve(ctx, c.Query, retrieve.Options{Budget: budget, Economics: &economics})
		record("configured retrieval", err)
		if len(cards) > 0 {
			cr.MeshTokens = retrieve.TotalTokens(cards) + readBody("configured", cards[0].Path)
		}
		cr.MeshMillis = elapsedMillis(started)
		record("evaluation context", ctx.Err())
		cr.RerankRoute = economics.Route

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
		for i, card := range cards {
			if i >= 5 {
				break
			}
			if want[card.NodeID] {
				cr.RerankTop5Surfaced = true
			}
		}
		for i, card := range localCards {
			if i >= 5 {
				break
			}
			if want[card.NodeID] {
				cr.LocalMeshSurfaced = true
			}
		}

		// Answer@1: the one body each arm reads.
		if len(fts) > 0 {
			cr.FTSAnswer1 = want[fts[0].NodeID]
		}
		if len(cards) > 0 {
			cr.MeshAnswer1 = want[cards[0].NodeID]
		}
		if len(localCards) > 0 {
			cr.LocalMeshAnswer1 = want[localCards[0].NodeID]
		}
		cr.RerankTokens = economics.ModelTokens()
		cr.CombinedTokens = cr.MeshTokens + cr.RerankTokens

		rep.Cases = append(rep.Cases, cr)
		for _, failure := range cr.Errors {
			rep.Errors = append(rep.Errors, fmt.Sprintf("case %d: %s", caseIndex+1, failure))
		}
		if len(cr.Errors) == 0 {
			meshMS = append(meshMS, cr.MeshMillis)
			localMS = append(localMS, cr.LocalMeshMillis)
			fts1MS = append(fts1MS, cr.FTSTop1Millis)
			fts3MS = append(fts3MS, cr.FTSTop3Millis)
		}
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
		if cr.LocalMeshSurfaced {
			rep.LocalMeshSurfaced++
		}
		if cr.LocalMeshAnswer1 {
			rep.LocalMeshAnswer1++
		}
		if cr.RerankTop5Surfaced {
			rep.RerankTop5Surfaced++
		}
		if economics.RerankCalled {
			rep.RerankCalls++
		}
		if economics.CacheHit {
			rep.RerankCacheHits++
		}
		if economics.Fallback {
			rep.RerankFallbacks++
		}
		rep.RerankInputTokens += economics.RerankInput
		rep.RerankOutputTokens += economics.RerankOutput
		rep.RerankTokens += economics.ModelTokens()
		if economics.ProviderReported {
			rep.ProviderReportedCalls++
		}
		mesh = append(mesh, cr.MeshTokens)
		fts1 = append(fts1, cr.FTSTop1Tokens)
		fts3 = append(fts3, cr.FTSTop3Tokens)
		localMesh = append(localMesh, cr.LocalMeshTokens)
		combined = append(combined, cr.CombinedTokens)
	}

	rep.MeshMean, rep.MeshMedian = mean(mesh), median(mesh)
	rep.FTSTop1Mean, rep.FTSTop1Median = mean(fts1), median(fts1)
	rep.FTSTop3Mean, rep.FTSTop3Median = mean(fts3), median(fts3)
	rep.LocalMeshMean, rep.LocalMeshMedian = mean(localMesh), median(localMesh)
	rep.CombinedMean, rep.CombinedMedian = mean(combined), median(combined)
	rep.Valid = len(rep.Errors) == 0 && rep.N > 0
	rep.MeshLatency, rep.LocalMeshLatency = summarizeLatency(meshMS), summarizeLatency(localMS)
	rep.FTSTop1Latency, rep.FTSTop3Latency = summarizeLatency(fts1MS), summarizeLatency(fts3MS)
	rep.RerankEvaluated = r.RerankActive()
	if rep.RerankEvaluated {
		rep.RerankQualityWin = rep.Valid && rep.RerankTop5Surfaced >= rep.LocalMeshSurfaced && rep.MeshAnswer1 >= rep.LocalMeshAnswer1
		rep.RerankCostWin = rep.Valid && rep.CombinedMedian < rep.LocalMeshMedian
		rep.RerankPass = rep.RerankQualityWin && rep.RerankCostWin && rep.RerankFallbacks == 0
	}

	rep.SurfacingWin = rep.Valid && rep.MeshSurfaced >= rep.FTSSurfaced
	rep.AnswerWin = rep.Valid && rep.MeshAnswer1 >= rep.FTSAnswer1
	rep.NaiveCostWin = rep.Valid && rep.MeshMedian < rep.FTSTop3Median
	rep.Pass = rep.SurfacingWin && rep.AnswerWin && rep.NaiveCostWin
	return rep
}

func bodyTokens(vaultRoot, relPath string) (int, error) {
	if !filepath.IsLocal(relPath) {
		return 0, fmt.Errorf("invalid vault-relative note path %q", relPath)
	}
	data, err := os.ReadFile(filepath.Join(vaultRoot, relPath))
	if err != nil {
		return 0, err
	}
	return retrieve.EstimateTokens(string(data)), nil
}

func elapsedMillis(start time.Time) float64 {
	return float64(time.Since(start)) / float64(time.Millisecond)
}

func summarizeLatency(values []float64) LatencySummary {
	if len(values) == 0 {
		return LatencySummary{}
	}
	s := append([]float64(nil), values...)
	sort.Float64s(s)
	n := len(s)
	median := s[n/2]
	if n%2 == 0 {
		median = (s[n/2-1] + s[n/2]) / 2
	}
	return LatencySummary{Samples: n, MedianMillis: median, P95Millis: s[int(math.Ceil(0.95*float64(n)))-1]}
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
