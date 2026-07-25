// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/tokenize"
)

// wireTokens is the ground truth: the tokens the caller actually pays for, counted
// on the exact JSON both mesh_search and GET /api/search marshal.
func wireTokens(cards []Card) int {
	b, err := json.Marshal(cards)
	if err != nil {
		return 0
	}
	return tokenize.Count(string(b))
}

// productionShapedCards builds n cards with realistic field widths (the old counter's
// undercount is proportional to the uncounted fields, so the shape matters).
func productionShapedCards(n int) []Card {
	cards := make([]Card, n)
	for i := range cards {
		s := strconv.Itoa(i)
		cards[i] = Card{
			NodeID:  "note:project-deploy-ci-" + s,
			NoteID:  "project-deploy-ci-" + s,
			Title:   "Deploy system is CI, Forgejo retired " + s,
			Path:    "projects/project_deploy_ci_" + s + ".md",
			Type:    "decision",
			Scope:   "dev",
			Snippet: strings.Repeat("post-receive on host is [CI] enqueueing builds ... ", 3),
			Score:   1.2345678901234567 / float64(i+1),
			Tier0:   i%3 == 0,
			Reason:  "fts +reranked",
		}
	}
	return cards
}

// TestCardTokensCountsTheSerializedCard is the regression for the flat 8-token
// structural allowance: the counter has to price the whole marshaled card (field
// names, ids, type, scope, tier0, the full-precision score), not four strings.
func TestCardTokensCountsTheSerializedCard(t *testing.T) {
	tests := []struct {
		name string
		card Card
	}{
		{name: "full corpus-shaped card", card: productionShapedCards(1)[0]},
		{name: "compact card with no snippet", card: func() Card {
			c := productionShapedCards(1)[0]
			c.Snippet = ""
			return c
		}()},
		{name: "empty card still costs its structure", card: Card{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			want := wireTokens([]Card{tc.card})
			got := cardTokens(tc.card)
			// The single-card wire form adds the enclosing array brackets, so allow a
			// couple of tokens of slack, but the count must not be materially short.
			if got < want-2 {
				t.Errorf("cardTokens = %d, understates the %d tokens actually serialized", got, want)
			}
		})
	}
}

// TestPackToBudgetRespectsTheRealPayload is the end of the same defect: because
// the count was ~1.7x short, the packer fitted ~1.7x more cards than the caller
// asked for and reported a token total that was wrong by the same factor.
func TestPackToBudgetRespectsTheRealPayload(t *testing.T) {
	tests := []struct {
		name   string
		cards  []Card
		budget int
	}{
		{name: "many cards, small budget", cards: productionShapedCards(40), budget: 2000},
		{name: "many cards, tiny budget", cards: productionShapedCards(40), budget: 300},
		{name: "few cards, generous budget", cards: productionShapedCards(3), budget: 5000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			packed := packToBudget(tc.cards, tc.budget)
			if len(packed) == 0 {
				t.Fatal("packer must always return at least one card")
			}
			actual := wireTokens(packed)
			// One card is always returned even if it alone busts the budget (the
			// documented "never return empty" floor), so only check the multi-card case.
			if len(packed) > 1 && actual > tc.budget {
				t.Errorf("packed payload is %d tokens, over the %d budget with %d cards", actual, tc.budget, len(packed))
			}
			if reported := TotalTokens(packed); reported < actual-len(packed)-2 {
				t.Errorf("reported %d tokens for a payload that really costs %d", reported, actual)
			}
		})
	}
}
