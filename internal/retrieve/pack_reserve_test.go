// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

// tier0AtTheBottom builds n production-shaped cards where only the two lowest-ranked
// ones are tier-0. Ordinary notes therefore win every token by score, and the only
// thing that can put institutional memory in front of the caller is the reserve the
// packer documents.
func tier0AtTheBottom(n int) []Card {
	cards := make([]Card, n)
	for i := range cards {
		s := strconv.Itoa(i)
		tier0 := i >= n-2
		typ := "note"
		if tier0 {
			typ = "decision"
		}
		cards[i] = Card{
			NodeID:  "note:project-deploy-pipeline-" + s,
			NoteID:  "project-deploy-pipeline-" + s,
			Title:   "Deploy system is the build pipeline, the old runner retired " + s,
			Path:    "projects/project_deploy_pipeline_" + s + ".md",
			Type:    typ,
			Scope:   "dev",
			Snippet: strings.Repeat("the post-receive hook enqueues a [build] on the server ... ", 3),
			Score:   1.2345678901234567 / float64(i+1),
			Tier0:   tier0,
			Reason:  "fts",
		}
	}
	return cards
}

// TestTier0ReserveHoldsACardAtSmallBudgets is the regression for reserve =
// budget/5. Measured on this fixture with the packer's own cardTokens, a card is 127
// tokens, so a fifth of any budget under 635 could not hold one and the reserve
// silently held nothing: budget 200 gave 1 card and 0 tier-0, budget 300 gave 2 cards
// and 0 tier-0, budget 400 gave 3 cards and 0 tier-0, on a set with two tier-0
// candidates. Only a budget <= 0 gets a default (8000 at the two search surfaces, 3000
// at ask and curator), so caller-supplied small budgets pass straight through.
//
// An earlier version of this comment claimed 73-76 tokens per card and a ~380
// threshold, and a later one said nothing in the package reproduced those. Both were
// wrong. Measured here (TestCountersAgreeOnWhatACardCosts pins them): the marshaled
// card is 127 tokens and the compact form 81, while the field-sum counter that used to
// live in cardTokens' marshal-failure path returned 75 and 29 for the same two cards.
// 75 is where the 73-76 came from. The package really did carry two counters that
// disagreed by 41% on a full card and 64% on a compact one, and the derived numbers
// were wrong only because the wrong one of the two was quoted. There is one counter now.
func TestTier0ReserveHoldsACardAtSmallBudgets(t *testing.T) {
	cards := tier0AtTheBottom(12)
	tests := []struct {
		name   string
		budget int
	}{
		{name: "budget 200", budget: 200},
		{name: "budget 300", budget: 300},
		{name: "budget 400", budget: 400},
		{name: "budget 8000 (the default)", budget: 8000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			packed := packToBudget(cards, tc.budget)
			var tier0 int
			for _, c := range packed {
				if c.Tier0 {
					tier0++
				}
			}
			if tier0 == 0 {
				t.Errorf("budget %d packed %d cards and no tier-0 card, so the reserve held "+
					"nothing and ordinary notes crowded institutional memory out", tc.budget, len(packed))
			}
			if got := TotalTokens(packed); len(packed) > 1 && got > tc.budget {
				t.Errorf("packed %d tokens over the %d budget", got, tc.budget)
			}
		})
	}
}

// TestReserveNeverStarvesOrdinaryCards guards the other side of the same change: the
// reserve is a floor on what tier-0 may claim, not a cap on the budget, so ordinary
// cards must still fill the rest.
func TestReserveNeverStarvesOrdinaryCards(t *testing.T) {
	cards := tier0AtTheBottom(12)
	packed := packToBudget(cards, 400)
	var ordinary int
	for _, c := range packed {
		if !c.Tier0 {
			ordinary++
		}
	}
	if ordinary == 0 {
		t.Errorf("the tier-0 reserve swallowed the whole budget: %d cards, none ordinary", len(packed))
	}
	if got := TotalTokens(packed); got > 400 {
		t.Errorf("packed %d tokens over the 400 budget", got)
	}
}

// TestBudgetBelowOneCompactCardReturnsEmpty pins Budget as a hard upper bound. The
// former never-return-empty floor deliberately exceeded small budgets, which made the
// caller's context cap advisory rather than enforceable.
func TestBudgetBelowOneCompactCardReturnsEmpty(t *testing.T) {
	cards := tier0AtTheBottom(3)
	compactCost := TotalTokens([]Card{compact(cards[0])})
	tests := []struct {
		name   string
		budget int
	}{
		{name: "budget far under one card", budget: 10},
		{name: "one token under compact card", budget: compactCost - 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if compactCost <= tc.budget {
				t.Skipf("precondition: a compact card must not fit, costs %d for budget %d", compactCost, tc.budget)
			}
			packed := packToBudget(cards, tc.budget)
			if len(packed) != 0 {
				t.Fatalf("budget %d cannot hold the cheapest card (%d tokens), got %d cards costing %d",
					tc.budget, compactCost, len(packed), TotalTokens(packed))
			}
		})
	}
}

// TestCountersAgreeOnWhatACardCosts is the regression for the package holding TWO
// counters. cardTokens prices the marshaled card, but its marshal-failure path priced
// title+path+snippet+reason+8, and the packer trusts whichever one answers: on this
// fixture that was 75 against 127 for a full card and 29 against 81 for a compact one,
// so a single malformed card re-opened the exact overrun cardTokens exists to close.
//
// Two things are pinned. The fallback must agree with the marshaled count (over is
// safe, under is not: an under-priced card looks free and the packer takes it plus
// everything after it). And a card whose Score cannot be encoded must be priced at what
// the SAME card costs with a finite score, because a non-finite score is a ranking bug,
// not a discount.
func TestCountersAgreeOnWhatACardCosts(t *testing.T) {
	full := tier0AtTheBottom(3)[0]
	tests := []struct {
		name string
		card Card
	}{
		{name: "full production-shaped card", card: full},
		{name: "superseded card includes optional receipt", card: func() Card {
			c := full
			c.SupersededBy = "replacement-with-a-long-stable-id"
			return c
		}()},
		{name: "compact card with no snippet", card: compact(full)},
		{name: "empty card is all structure", card: Card{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			want := cardTokens(tc.card)
			got := cardTokensNoMarshal(tc.card)
			if got < want-2 {
				t.Errorf("the no-marshal counter prices this card at %d, understating the %d "+
					"the marshaled form really costs: the packer over-fills by that ratio "+
					"whenever it takes that path", got, want)
			}
			if got > want+want/5+16 {
				t.Errorf("the no-marshal counter prices this card at %d against a real %d, "+
					"so far over that it would starve the budget", got, want)
			}
		})
	}

	scores := []struct {
		name  string
		score float64
	}{
		{name: "NaN score", score: math.NaN()},
		{name: "+Inf score", score: math.Inf(1)},
		{name: "-Inf score", score: math.Inf(-1)},
	}
	for _, tc := range scores {
		t.Run(tc.name, func(t *testing.T) {
			bad := full
			bad.Score = tc.score
			zero := full
			zero.Score = 0
			if got, want := cardTokens(bad), cardTokens(zero); got != want {
				t.Errorf("a card with a %s is priced at %d, but the same card with a finite "+
					"score costs %d: an unencodable score must not change the accounting",
					tc.name, got, want)
			}
		})
	}
}
