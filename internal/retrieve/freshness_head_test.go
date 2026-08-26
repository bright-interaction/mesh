// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"
)

// TestFreshnessDecayReachesTheRerankedHead is the regression for the freshness
// signal being dead whenever a reranker is configured.
//
// rerankHead ASSIGNS head[i].Score rather than adjusting it, so every multiplier the
// card loop folded in was discarded, and rerankK (30) is larger than the default
// Limit (20), which makes the head the whole returned set. Repro before the fix: a
// two-note vault with the reranker active scored identically at freshHalfLife 0 and
// freshHalfLife 30, so MESH_FRESHNESS_HALFLIFE_DAYS bought a reranking deployment
// nothing at all.
func TestFreshnessDecayReachesTheRerankedHead(t *testing.T) {
	stale := time.Now().AddDate(-10, 0, 0).Format("2006-01-02")
	fresh := time.Now().Format("2006-01-02")
	const query = "alpha beta gamma delta epsilon"
	srcs := []noteSrc{
		{"astale.md", "---\nid: astale\ntype: note\nwhen: " + stale + "\nupdated: " + stale +
			"\n---\n# Stale\n" + query + " stalemarker\n"},
		{"zfresh.md", "---\nid: zfresh\ntype: note\nwhen: " + fresh + "\nupdated: " + fresh +
			"\n---\n# Fresh\n" + query + "\n"},
	}

	// scoreOfStale runs one retrieval and returns the stale note's final score plus
	// its reason, so the test can also prove the reranked path was really taken.
	scoreOfStale := func(t *testing.T, halfLife int, reranking bool) (float64, string) {
		t.Helper()
		r := buildVaultFrom(t, srcs)
		r.freshHalfLife = halfLife
		if reranking {
			// The reranker prefers the STALE note, so it lands at the top of the head
			// with a normalized relevance of 1. That keeps the assertion about the
			// freshness multiplier alone, independent of where the floor sits.
			r.EnableRerank(fakeReranker{needle: "stalemarker"})
		}
		cards, err := r.Retrieve(context.Background(), query, Options{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cards {
			if c.NodeID == "note:astale" {
				return c.Score, c.Reason
			}
		}
		t.Fatalf("stale note missing from results %v", cardIDs(cards))
		return 0, ""
	}

	tests := []struct {
		name       string
		reranking  bool
		wantRerank bool
	}{
		{name: "reranked head", reranking: true, wantRerank: true},
		{name: "fused tail, no reranker configured", reranking: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			off, _ := scoreOfStale(t, 0, tc.reranking)
			on, reason := scoreOfStale(t, 30, tc.reranking)
			if tc.wantRerank && !strings.Contains(reason, "reranked") {
				t.Fatalf("precondition: the card must go through the rerank head, reason = %q", reason)
			}
			if !(on < off) {
				t.Errorf("a 10-year-old note scored %f with freshness on and %f with it off: "+
					"the freshness multiplier never reached this card", on, off)
			}
		})
	}
}

// TestRerankBlendAppliesTier0Exactly is the regression for the tier-0 boost being
// folded in twice whenever MESH_RERANK_BLEND < 1.0: rerankHead read its fused
// component back off head[i].Score, which already carried the 1.1x from the card
// loop, and then multiplied by tier0Mult again for an effective 1.21x on the fused
// half of the blend. Inert at the shipped blend of 1.0, live for anyone using the
// documented knob.
//
// rerankHead is driven directly so the arithmetic is exact: the two runs differ only
// in the tier-0 flag, and everything the blend reads (the reranker's scores and the
// pre-boost fused scores) is held identical.
func TestRerankBlendAppliesTier0Exactly(t *testing.T) {
	// The tier-0 card is BEHIND the plain card on the raw fused signal (0.47 vs 0.50)
	// and behind it on the reranker too. Its boosted score, 0.47*1.1 = 0.517, is what
	// put it first in the incoming order, and that is exactly the number the old code
	// fed back into the fused normalization.
	const rawBoosted, rawPlain = 0.47, 0.50
	fusedRaw := map[string]float64{"note:boosted": rawBoosted, "note:plain": rawPlain}

	// run returns the boosted card's relevance above the base offset, for a given
	// tier-0 flag. The incoming order is held fixed so the head is byte-identical
	// apart from the flag under test.
	run := func(t *testing.T, tier0 bool) (rel float64, top string) {
		t.Helper()
		boostedType := "note"
		if tier0 {
			boostedType = "decision"
		}
		srcs := []noteSrc{
			{"boosted.md", "---\nid: boosted\ntype: " + boostedType + "\nwhen: 2026-01-01\n---\n# Boosted\nstorage engine note\n"},
			{"plain.md", "---\nid: plain\ntype: note\nwhen: 2026-01-01\n---\n# Plain\nstorage engine plainmarker\n"},
		}
		r := buildVaultFrom(t, srcs)
		r.EnableRerank(fakeReranker{needle: "plainmarker"}) // the reranker prefers the PLAIN note
		r.rerankBlend = 0.5
		score := rawBoosted
		if tier0 {
			score *= tier0Mult
		}
		cards := []Card{
			{NodeID: "note:boosted", NoteID: "boosted", Title: "Boosted", Type: "decision", Tier0: tier0, Score: score},
			{NodeID: "note:plain", NoteID: "plain", Title: "Plain", Type: "note", Score: rawPlain},
		}
		var err error
		cards, err = r.rerankHead(context.Background(), "storage engine", cards, fusedRaw, Options{})
		if err != nil {
			t.Fatal(err)
		}
		// base is max(tail)+1 and the tail is empty here, so it is exactly 1.
		for _, c := range cards {
			if c.NodeID == "note:boosted" {
				rel = c.Score - 1
			}
		}
		return rel, cards[0].NodeID
	}

	boosted, top := run(t, true)
	plainForm, _ := run(t, false)
	if got, want := boosted, plainForm*tier0Mult; math.Abs(got-want) > 1e-9 {
		t.Errorf("tier-0 relevance = %v, want %v (exactly %vx the un-boosted form, got %vx)",
			got, want, tier0Mult, got/plainForm)
	}
	// The consequence, stated as ranking: a tier-0 card that loses on the raw fused
	// signal AND loses to the reranker must not come out on top.
	if top != "note:plain" {
		t.Errorf("top card = %s, want note:plain: the tier-0 card lost both signals and "+
			"still won, which is the compounded boost", top)
	}
}

// TestWeakestSignalMatchStillCarriesTheTier0Nudge is the regression for the boost
// being a no-op over part of its own domain. minMax maps the weakest candidate of
// every signal to exactly 0, and 0 * tier0Mult == 0, so a decision note that was the
// weakest of three FTS matches scored 0.000000 and sorted dead last on the
// alphabetical NodeID tie-break with its institutional-memory boost buying nothing.
// Same shape as the historic 0.6 freshness clamp.
func TestWeakestSignalMatchStillCarriesTheTier0Nudge(t *testing.T) {
	const body = "alpha beta gamma delta epsilon"
	srcs := []noteSrc{
		{"strong.md", "---\nid: strong\ntype: note\nwhen: 2026-01-01\n---\n# Strong hit\n" +
			body + " " + body + " " + body + "\n"},
		// Identical searchable text and equal-length titles, so these two tie at the
		// bottom of the FTS range and only the tier-0 nudge can separate them.
		{"aplain.md", "---\nid: aplain\ntype: note\nwhen: 2026-01-01\n---\n# Weak one\n" + body + "\n"},
		{"zdecision.md", "---\nid: zdecision\ntype: decision\nwhen: 2026-01-01\n---\n# Weak two\n" + body + "\n"},
	}
	r := buildVaultFrom(t, srcs)
	cards, err := r.Retrieve(context.Background(), body,
		Options{Limit: 10, WeightFTS: 1, NoRerank: true})
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, c := range cards {
		pos[c.NodeID] = i
		if c.Score <= 0 {
			t.Errorf("card %s scored %f: the weakest match of a signal normalizes to a dead "+
				"zero, so every multiplicative boost is a no-op on it", c.NodeID, c.Score)
		}
	}
	dec, okDec := pos["note:zdecision"]
	plain, okPlain := pos["note:aplain"]
	if !okDec || !okPlain {
		t.Fatalf("precondition: both weak notes must be returned, got %v", cardIDs(cards))
	}
	if dec > plain {
		t.Errorf("the tier-0 note ranked %d, behind the equally-weak plain note at %d: "+
			"the institutional-memory nudge had nothing to act on (%v)", dec, plain, cards)
	}
}
