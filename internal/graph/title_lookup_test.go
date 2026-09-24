// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package graph

import (
	"strings"
	"testing"
)

func TestMatchesFullTitle(t *testing.T) {
	for _, tc := range []struct {
		query, title string
		want         bool
	}{
		{"Orbit v1.2.3 activated", "Orbit v1.2.3 activated", true},
		{" \nOrbit v1.2.3 activated\t", "ORBIT V1.2.3 ACTIVATED", true},
		{"ÅTGÄRD FÖR Σ v1.2.3", "åtgärd för ς v1.2.3", true},
		{"", "", false},
		{" \t", " \t", false},
		{"Orbit v1.2.3 activated", "Orbit v1.2.30 activated", false},
		{"Orbit v1.2.3 activated", "Orbit v1.2.3-rc.1 activated", false},
		{"Orbit v1.2.3 activated", "Orbit v1.2.3+build.1 activated", false},
		{"Orbit v1.2.3 activated", "Orbit v1-2-3 activated", false},
		{"allow extraction", "do not allow extraction", false},
		{"allow extraction", "allow  extraction", false},
		{strings.Repeat("alpha ", 100) + "deny", strings.Repeat("alpha ", 100) + "allow", false},
	} {
		if got := MatchesFullTitle(tc.query, tc.title); got != tc.want {
			t.Errorf("MatchesFullTitle(%q,%q)=%t, want %t", tc.query, tc.title, got, tc.want)
		}
	}
}
