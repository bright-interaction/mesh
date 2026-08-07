// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"encoding/json"
	"math"

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
//
// The marshal-failure path used to keep that same field sum, so the package still
// carried TWO counters that disagreed about what a card costs: measured on the
// pack_reserve_test.go fixture, the field sum priced a full card at 75 against the
// marshaled 127 and a compact card at 29 against 81, i.e. 41% and 64% short. That is
// not a safe degradation, it is the original overrun re-entering through the one path
// meant to keep the packer moving. There is now one counter: the only field that can
// fail to encode is a non-finite Score (everything else is a string or a bool), so
// that field is sanitized and the SAME wire shape is priced.
func cardTokens(c Card) int {
	if math.IsNaN(c.Score) || math.IsInf(c.Score, 0) {
		c.Score = 0
	}
	b, err := json.Marshal(c)
	if err != nil {
		return cardTokensNoMarshal(c)
	}
	return estimateTokens(string(b))
}

// cardTokensNoMarshal prices a card without marshaling it. Unreachable in practice
// (cardTokens sanitizes the only unencodable field first), but a counter that returns
// 0 makes a card look free and lets the packer take everything, so it stays. It is
// built on the same basis as the marshaled form rather than on a flat allowance: every
// string field plus the measured structure of an empty card.
// TestCountersAgreeOnWhatACardCosts pins the two within a few percent so they cannot
// drift apart again.
func cardTokensNoMarshal(c Card) int {
	n := 0
	// json.Marshal of a plain string cannot fail, and encoding each value the way the
	// marshaler would (quotes and escapes included) is what keeps this close to the
	// real wire count instead of ~10% under it: the quote is part of the token that
	// starts and ends every field.
	for _, s := range []string{c.NodeID, c.NoteID, c.Title, c.Path, c.Type, c.Scope, c.Snippet, c.Reason} {
		if b, err := json.Marshal(s); err == nil {
			n += estimateTokens(string(b))
		}
	}
	if b, err := json.Marshal(Card{}); err == nil { // the ten field names, the separators, Score, Tier0
		n += estimateTokens(string(b))
	}
	return n
}

// TotalTokens sums the estimated cost of a set of cards.
func TotalTokens(cards []Card) int { return TotalTokensFunc(cards, nil) }

// TotalTokensFunc sums a card set priced by cost (nil = cardTokens). A surface that
// packs with its own CardCost must report with the same one, otherwise the number it
// hands the caller is not the number it packed to.
func TotalTokensFunc(cards []Card, cost CardCost) int {
	if cost == nil {
		cost = cardTokens
	}
	n := 0
	for _, c := range cards {
		n += cost(c)
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
// small budgets (every caller below forwards any positive budget verbatim and only
// defaults a budget <= 0), so the "never crowded out by ordinary notes" promise held
// on large budgets only.
//
// Raising it cannot overrun: reserve is capped at the budget, and pass B still
// measures against the full budget.
//
// Between the compact and the full cost the reserve measures the COMPACT form,
// because packToBudget can emit that form and a reserve that only ever priced the
// full one held nothing across the whole band. Measured on the same fixture: a
// compact card is 81 tokens, so at budget 100 pass A used to take nothing and the
// caller got one ordinary compact card and zero tier-0 cards even though a compact
// tier-0 card fit. It now gets the tier-0 card.
//
// The full list of budget-forwarding callers, because two of them were missed when the
// reserve was first written up and someone will look for them here:
//
//	internal/mcp/tools.go        mesh_search, any positive budget verbatim, else 8000
//	internal/web/search_api.go   GET /api/search, same, else 8000
//	internal/ask/ask.go          Answer(), any positive budget verbatim, else 3000
//	internal/curator/curator.go  gatherContext(), Curator.Budget, else 3000
//	cmd/mesh/main.go             mesh search --budget, verbatim, NO default (0 = unpacked)
//	internal/eval/eval.go        mesh eval --budget, verbatim, NO default (0 = unpacked)
//
// The last two default to 0, so they skip the packer entirely rather than getting a
// default budget, and neither rewrites a card after packing: the CLI reports with
// TotalTokens over the same cards it prints, and eval measures the same set it scores.
//
// ask and curator ARE subject to this reserve (at their 3000 default it resolves to
// 600, comfortably more than the 127 a fixture card costs, so it holds; below ~635 the
// floor above is what saves them). Neither is subject to the mcp overrun that made the
// cost injectable: both render each card into prose and DROP most of it, measured at 73
// and 68 tokens against the 127 the packer charged, so they under-spend their budget
// rather than blowing it. Do not "fix" that by packing them with a cheaper cost: their
// renders are lossy, and the packer has to keep pricing the card it hands back.
func tier0Reserve(cards []Card, budget int, cost CardCost) int {
	reserve := budget / 5
	for _, c := range cards {
		if !c.Tier0 {
			continue
		}
		// Cards arrive sorted by score desc, so this is the best tier-0 card that can
		// fit at all. Price the full form first and fall back to the compact one, which
		// is what pass A will place when the full form overruns the whole budget. A
		// card whose compact form still does not fit cannot be reserved for at all.
		n := cost(c)
		if n > budget {
			n = cost(compact(c))
			if n > budget {
				continue
			}
		}
		if n > reserve {
			reserve = n
		}
		break
	}
	return reserve
}

// compact is the no-snippet form of a card: the degraded shape both packing passes
// fall back to when the full form will not fit.
func compact(c Card) Card {
	c.Snippet = ""
	return c
}

// CardCost prices one card the way the CALLER will actually receive it. The default is
// cardTokens, which prices retrieve.Card's own JSON.
//
// It is injectable because a surface can put different bytes on the wire than the card
// carries, and a budget priced on bytes nobody sends is a lie: internal/mcp wraps the
// snippet of every connector-ingested card in a provenance envelope and adds a Source
// field AFTER retrieval, which measured 41% over an 8000-token budget (11284 tokens
// sent against 7988 reported) before that surface started packing with its own cost.
// If you rewrite a card after packing, pass the cost of the rewritten form here
// instead, and report with TotalTokensFunc using the same function.
type CardCost func(Card) int

// packToBudget packs with the plain card cost: the retriever's own callers get exactly
// the bytes the packer priced.
func packToBudget(cards []Card, budget int) []Card { return PackToBudget(cards, budget, nil) }

// PackToBudget selects the highest-scoring cards that fit the token budget,
// reserving part of it (see tier0Reserve) for the institutional-memory tier so
// decisions, gotchas, and post-mortems are never crowded out by ordinary notes.
// Input is assumed sorted by score desc; output preserves that order.
// cost prices a card (nil = cardTokens); see CardCost.
func PackToBudget(cards []Card, budget int, cost CardCost) []Card {
	if cost == nil {
		cost = cardTokens
	}
	if budget <= 0 {
		return cards
	}
	reserve := tier0Reserve(cards, budget, cost)
	used := 0
	picked := make([]Card, len(cards))
	taken := make([]bool, len(cards))

	// cost marshals the card, so each call is a real BPE tokenization. Measure each
	// form once and reuse the number instead of re-counting it.
	// Pass A: reserve room for the best tier-0 cards. Full form first; when the full
	// form does not fit the whole BUDGET, fall back to the compact one, matching what
	// pass B would emit. The budget test is what keeps this from costing snippets: a
	// full card the budget can afford is left for pass B to place whole, so the
	// degradation only ever happens where the alternative is no tier-0 card at all.
	for i, c := range cards {
		if !c.Tier0 {
			continue
		}
		n := cost(c)
		if used+n <= reserve {
			picked[i] = c
			taken[i] = true
			used += n
			continue
		}
		if n <= budget {
			continue
		}
		cc := compact(c)
		if cn := cost(cc); used+cn <= reserve {
			picked[i] = cc
			taken[i] = true
			used += cn
		}
	}
	// Pass B: fill the rest by score. When a full card will not fit, degrade it
	// to a compact (no-snippet) form rather than skipping to a lower-ranked
	// card, so the best results always win the budget.
	for i, c := range cards {
		if taken[i] {
			continue
		}
		if n := cost(c); used+n <= budget {
			picked[i] = c
			taken[i] = true
			used += n
			continue
		}
		cc := compact(c)
		if n := cost(cc); used+n <= budget {
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
		if cost(best) > budget {
			best = compact(best)
		}
		out = append(out, best)
	}
	return out
}
