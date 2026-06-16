package retrieve

// estimateTokens approximates the token count of a string. Words contribute
// ~one token per 4 characters (subword splitting), and each punctuation or
// symbol is its own token. This beats the chars/3.5 heuristic the review
// flagged as ~1.6x off on dense markdown. Milestone 1 step 5 swaps in a vendored
// BPE tokenizer for Gate-1-accurate measurement; the packer reads it through
// this one function so the swap is local.
func estimateTokens(s string) int {
	tokens := 0
	wlen := 0
	flush := func() {
		if wlen > 0 {
			tokens += (wlen + 3) / 4
			wlen = 0
		}
	}
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			wlen++
		case r == ' ' || r == '\n' || r == '\t' || r == '\r':
			flush()
		default:
			flush()
			tokens++
		}
	}
	flush()
	return tokens
}

// cardTokens estimates the rendered cost of a card (title + path + snippet +
// reason + a little structural overhead).
func cardTokens(c Card) int {
	return estimateTokens(c.Title) + estimateTokens(c.Path) +
		estimateTokens(c.Snippet) + estimateTokens(c.Reason) + 8
}

// TotalTokens sums the estimated cost of a set of cards.
func TotalTokens(cards []Card) int {
	n := 0
	for _, c := range cards {
		n += cardTokens(c)
	}
	return n
}

// packToBudget selects the highest-scoring cards that fit the token budget,
// reserving a fifth of it for the institutional-memory tier so decisions,
// gotchas, and post-mortems are never crowded out by ordinary notes. Input is
// assumed sorted by score desc; output preserves that order.
func packToBudget(cards []Card, budget int) []Card {
	if budget <= 0 {
		return cards
	}
	reserve := budget / 5
	used := 0
	picked := make([]Card, len(cards))
	taken := make([]bool, len(cards))

	// Pass A: reserve room for the best tier-0 cards (full form).
	for i, c := range cards {
		if c.Tier0 && used+cardTokens(c) <= reserve {
			picked[i] = c
			taken[i] = true
			used += cardTokens(c)
		}
	}
	// Pass B: fill the rest by score. When a full card will not fit, degrade it
	// to a compact (no-snippet) form rather than skipping to a lower-ranked
	// card, so the best results always win the budget.
	for i, c := range cards {
		if taken[i] {
			continue
		}
		if used+cardTokens(c) <= budget {
			picked[i] = c
			taken[i] = true
			used += cardTokens(c)
			continue
		}
		cc := c
		cc.Snippet = ""
		if used+cardTokens(cc) <= budget {
			picked[i] = cc
			taken[i] = true
			used += cardTokens(cc)
		}
	}

	out := make([]Card, 0, len(cards))
	for i := range cards {
		if taken[i] {
			out = append(out, picked[i])
		}
	}
	// Never return empty when a relevant note exists: hand back the best card.
	if len(out) == 0 && len(cards) > 0 {
		out = append(out, cards[0])
	}
	return out
}
