// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import "testing"

// TestTier0ReserveHoldsACompactCardInTheDegradeBand is the regression for the
// residual the previous reserve fix documented but did not close: pass A priced only
// the FULL form of a tier-0 card, while pass B is free to emit the compact
// (no-snippet) one. Across the whole band [compact cost, full cost) the reserve
// therefore held nothing, and the caller got an ordinary compact card and zero tier-0
// cards. Measured on this fixture with the packer's own cardTokens: full 127 tokens,
// compact 81, so budget 100 sat inside the band and returned no institutional memory.
//
// The band is derived from the fixture rather than hardcoded, so the test keeps
// meaning if the fixture's shape changes.
func TestTier0ReserveHoldsACompactCardInTheDegradeBand(t *testing.T) {
	cards := tier0AtTheBottom(12)
	full := cardTokens(cards[0])
	small := cardTokens(compact(cards[0]))
	if small >= full {
		t.Fatalf("precondition: the compact form must be cheaper than the full one (compact %d, full %d)", small, full)
	}

	tests := []struct {
		name   string
		budget int
	}{
		{name: "exactly one compact card", budget: small},
		{name: "just above one compact card", budget: small + 1},
		{name: "mid band", budget: (small + full) / 2},
		{name: "one token under a full card", budget: full - 1},
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
				t.Errorf("budget %d is in [%d compact, %d full) and packed %d cards with no tier-0 card, "+
					"so the reserve held nothing even though a compact tier-0 card fit",
					tc.budget, small, full, len(packed))
			}
			if got := TotalTokens(packed); got > tc.budget {
				t.Errorf("packed %d tokens over the %d budget", got, tc.budget)
			}
		})
	}
}

// TestReserveDoesNotDegradeWhatTheBudgetCanAfford guards the other side of the same
// change. Degrading in pass A is only correct where the alternative is no tier-0 card
// at all: whenever the full form fits the budget, pass B can still place it whole, so
// the packer must not spend a snippet it could have kept.
func TestReserveDoesNotDegradeWhatTheBudgetCanAfford(t *testing.T) {
	cards := tier0AtTheBottom(12)
	full := cardTokens(cards[0])
	small := cardTokens(compact(cards[0]))

	// A budget whose fifth leaves room inside the reserve for a compact card but not
	// a full one, while the budget itself affords the full form many times over. This
	// is the case a blanket degrade-in-pass-A would get wrong.
	reserveWithARemainder := (full + (small+full)/2) * 5

	tests := []struct {
		name   string
		budget int
	}{
		{name: "exactly one full card", budget: full},
		{name: "reserve remainder fits only a compact card", budget: reserveWithARemainder},
		{name: "the 8000 default", budget: 8000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			packed := packToBudget(cards, tc.budget)
			var tier0 int
			for _, c := range packed {
				if !c.Tier0 {
					continue
				}
				tier0++
				if c.Snippet == "" {
					t.Errorf("budget %d fits a full card (%d tokens) but the tier-0 card came back "+
						"compact, so pass A degraded a card the budget could afford", tc.budget, full)
				}
			}
			if tier0 == 0 {
				t.Errorf("budget %d packed %d cards and no tier-0 card", tc.budget, len(packed))
			}
			if got := TotalTokens(packed); got > tc.budget {
				t.Errorf("packed %d tokens over the %d budget", got, tc.budget)
			}
		})
	}
}
