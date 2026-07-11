// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"regexp"
	"testing"
)

func TestOKLCHToHex(t *testing.T) {
	hex := regexp.MustCompile(`^#[0-9a-f]{6}$`)
	for _, h := range []float64{0, 60, 120, 200, 300, 359} {
		got := oklchToHex(0.74, 0.135, h)
		if !hex.MatchString(got) {
			t.Fatalf("oklchToHex(.74,.135,%v) = %q, want #rrggbb", h, got)
		}
	}
	// Out-of-gamut inputs must clamp to valid channels, not overflow.
	if got := oklchToHex(1.2, 0.4, 30); !hex.MatchString(got) {
		t.Fatalf("out-of-gamut should clamp to a valid hex, got %q", got)
	}
}

func TestCommunityHueDistinct(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < topColored; i++ {
		c := communityHue(i)
		if seen[c] {
			t.Fatalf("communityHue(%d)=%q collided within the top set", i, c)
		}
		seen[c] = true
	}
	if communityHue(0) == tailGray {
		t.Fatal("a top-ranked community must not use the tail gray")
	}
}
