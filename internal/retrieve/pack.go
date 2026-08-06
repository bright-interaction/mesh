// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"encoding/json"

	"github.com/bright-interaction/mesh/internal/tokenize"
)

// estimateTokens returns the token count of a string using the real cl100k_base
// BPE tokenizer (internal/tokenize). The packer reads token cost through this one
// function, so budgeting and the Gate-1 measurement rest on a genuine count, not
// a chars-per-token estimate. The name is kept so the call sites are unchanged.
func estimateTokens(s string) int { return tokenize.Count(s) }

// EstimateTokens is the exported counter so the measurement harness counts both
// arms with the exact same function the packer uses.
func EstimateTokens(s string) int { return estimateTokens(s) }

// cardTokens is the token cost of a card as the caller actually receives it.
//
// It counts the marshaled JSON, not a hand-picked subset of the fields. The old
// form summed title+path+snippet+reason and allowed a flat 8 tokens for
// "structure", but the wire form also carries NodeID, NoteID, Type, Scope, Tier0,
// a full-precision Score and ten Go field names, which measured at roughly 60
// tokens per card. The packer therefore fit about 1.7x more cards than the
// caller's budget allowed and reported a token total short by the same factor.
func cardTokens(c Card) int {
	b, err := json.Marshal(c)
	if err != nil {
		// A card that cannot be marshaled cannot be sent either (a NaN Score is the
		// only realistic cause). Fall back to the field sum so the packer keeps
		// making progress instead of treating the card as free.
		return estimateTokens(c.Title) + estimateTokens(c.Path) +
			estimateTokens(c.Snippet) + estimateTokens(c.Reason) + 8
	}
	return estimateTokens(string(b))
}

// TotalTokens sums the estimated cost of a set of cards.
func TotalTokens(cards []Card) int {
	n := 0
	for _, c := range cards {
		n += cardTokens(c)
	}
	return n
}

// tier0Reserve is the slice of the budget pass A may spend on institutional-memory
// cards: a fifth of it, but never less than the best tier-0 card actually costs.
//
// A flat budget/5 is not a reserve until the budget reaches FIVE times what a card
// costs, because that is when budget/5 first fits one. The fixture in
// pack_reserve_test.go measures 127 tokens by cardTokens (the same counter the packer
// uses), so the old reserve was 40 at budget 200, 60 at 300 and 80 at 400, and pass A
// could not fit a single tier-0 card in any of them: 1, 2 and 3 packed cards
// respectively, none tier-0, on a set holding two tier-0 candidates. Callers do pass
// small budgets (both the MCP tool and the /api/search handler forward any positive
// budget verbatim; only a budget <= 0 gets the 8000 default), so the "never crowded
// out by ordinary notes" promise held on large budgets only.
//
// Raising it cannot overrun: reserve is capped at the budget, and pass B still
// measures against the full budget.
//
// Known residual: below one FULL card's cost the reserve still holds nothing, because
// pass A only ever measures the full form while pass B may degrade a card to the
// compact (no-snippet) one. On the same fixture a budget of 100 returns one ordinary
// compact card and no tier-0 card, even though a compact tier-0 card would have fit.
// Closing that means teaching pass A to degrade too, which is a wider change than
// this reserve.
func tier0Reserve(cards []Card, budget int) int {
	reserve := budget / 5
	for _, c := range cards {
		if !c.Tier0 {
			continue
		}
		// Cards arrive sorted by score desc, so this is the best tier-0 card that can
		// fit at all. One that cannot fit the whole budget would not fit any reserve.
		n := cardTokens(c)
		if n > budget {
			continue
		}
		if n > reserve {
			reserve = n
		}
		break
	}
	return reserve
}

// packToBudget selects the highest-scoring cards that fit the token budget,
// reserving part of it (see tier0Reserve) for the institutional-memory tier so
// decisions, gotchas, and post-mortems are never crowded out by ordinary notes.
// Input is assumed sorted by score desc; output preserves that order.
func packToBudget(cards []Card, budget int) []Card {
	if budget <= 0 {
		return cards
	}
	reserve := tier0Reserve(cards, budget)
	used := 0
	picked := make([]Card, len(cards))
	taken := make([]bool, len(cards))

	// cardTokens now marshals the card, so each call is a real BPE tokenization.
	// Measure each form once and reuse the number instead of re-counting it.
	// Pass A: reserve room for the best tier-0 cards (full form).
	for i, c := range cards {
		n := cardTokens(c)
		if c.Tier0 && used+n <= reserve {
			picked[i] = c
			taken[i] = true
			used += n
		}
	}
	// Pass B: fill the rest by score. When a full card will not fit, degrade it
	// to a compact (no-snippet) form rather than skipping to a lower-ranked
	// card, so the best results always win the budget.
	for i, c := range cards {
		if taken[i] {
			continue
		}
		if n := cardTokens(c); used+n <= budget {
			picked[i] = c
			taken[i] = true
			used += n
			continue
		}
		cc := c
		cc.Snippet = ""
		if n := cardTokens(cc); used+n <= budget {
			picked[i] = cc
			taken[i] = true
			used += n
		}
	}

	out := make([]Card, 0, len(cards))
	for i := range cards {
		if taken[i] {
			out = append(out, picked[i])
		}
	}
	// Never return empty when a relevant note exists: hand back the best card. The
	// floor is deliberate (a caller with a 10-token budget still learns which note
	// answers the query, and TotalTokens reports the real cost), but the full form
	// overran the budget by the whole snippet for nothing. Degrade to the compact
	// form the passes above already prefer whenever the full one does not fit.
	if len(out) == 0 && len(cards) > 0 {
		best := cards[0]
		if cardTokens(best) > budget {
			best.Snippet = ""
		}
		out = append(out, best)
	}
	return out
}
