// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
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
// budget/5. A real card measures 73-76 tokens by the packer's own counter, so a
// fifth of any budget under ~380 could not hold one and the reserve silently held
// nothing: budget 200 gave 2 cards and 0 tier-0, budget 300 gave 4 cards and 0
// tier-0, on a set with two tier-0 candidates. Only a budget <= 0 gets the 8000
// default, so caller-supplied small budgets pass straight through.
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

// TestFallbackCardPrefersTheCompactForm is the regression for the never-return-empty
// floor appending a full-form card with no size check, so a 10-token budget was
// answered with the whole snippet. The floor itself is intentional (the caller still
// learns which note answers the query, and TotalTokens reports the real cost), but
// it must overrun by as little as it can.
func TestFallbackCardPrefersTheCompactForm(t *testing.T) {
	cards := tier0AtTheBottom(3)
	full := cardTokens(cards[0])
	tests := []struct {
		name   string
		budget int
	}{
		{name: "budget far under one card", budget: 10},
		{name: "budget under one full card", budget: 50},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if full <= tc.budget {
				t.Skipf("precondition: a full card must not fit, costs %d for budget %d", full, tc.budget)
			}
			packed := packToBudget(cards, tc.budget)
			if len(packed) != 1 {
				t.Fatalf("want exactly the one best card, got %d", len(packed))
			}
			if packed[0].Snippet != "" {
				t.Errorf("budget %d got the full-form card back (%d tokens, snippet intact) when "+
					"the compact form was available", tc.budget, TotalTokens(packed))
			}
			if got := TotalTokens(packed); got >= full {
				t.Errorf("fallback cost %d tokens, no cheaper than the full card's %d", got, full)
			}
		})
	}
}
