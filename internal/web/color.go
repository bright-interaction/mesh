// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"fmt"
	"math"
)

// tailGray is a warm neutral for communities beyond the top N: present but quiet,
// so the eye reads the big clusters as colored and the long tail as background.
const tailGray = "#7c766e"

// communityHue returns a perceptually-spaced color for the rank-th largest
// community (0-based), tuned for a dark canvas: fixed OKLCH lightness + chroma so
// every hue reads at the same weight, hue stepped by the golden angle so adjacent
// ranks land far apart on the wheel. Saturated, never neon (chroma is modest).
func communityHue(rank int) string {
	const L, C = 0.74, 0.135
	h := math.Mod(float64(rank)*137.508+20, 360)
	return oklchToHex(L, C, h)
}

// oklchToHex converts OKLCH (L 0..1, C chroma, H degrees) to an sRGB hex string.
// OKLCH -> OKLab -> linear sRGB (the standard matrices) -> gamma-encoded sRGB.
func oklchToHex(L, C, hDeg float64) string {
	hr := hDeg * math.Pi / 180
	a := C * math.Cos(hr)
	b := C * math.Sin(hr)

	l_ := L + 0.3963377774*a + 0.2158037573*b
	m_ := L - 0.1055613458*a - 0.0638541728*b
	s_ := L - 0.0894841775*a - 1.2914855480*b
	l := l_ * l_ * l_
	m := m_ * m_ * m_
	s := s_ * s_ * s_

	r := +4.0767416621*l - 3.3077115913*m + 0.2309699292*s
	gg := -1.2684380046*l + 2.6097574011*m - 0.3413193965*s
	bb := -0.0041960863*l - 0.7034186147*m + 1.7076147010*s

	return "#" + hex2(gammaEncode(r)) + hex2(gammaEncode(gg)) + hex2(gammaEncode(bb))
}

func gammaEncode(c float64) int {
	if c <= 0.0031308 {
		c = 12.92 * c
	} else {
		c = 1.055*math.Pow(c, 1.0/2.4) - 0.055
	}
	v := int(math.Round(c * 255))
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}

func hex2(v int) string { return fmt.Sprintf("%02x", v) }
