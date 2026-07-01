// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package vault

import "testing"

func TestScopeAllows(t *testing.T) {
	dev := map[string]bool{"dev": true}
	sales := map[string]bool{"sales": true}

	cases := []struct {
		name    string
		scopes  []string
		allowed map[string]bool
		want    bool
	}{
		{"nil allowed is unrestricted", []string{"dev"}, nil, true},
		{"dev caller reads dev note", []string{"dev"}, dev, true},
		{"sales caller cannot read dev note", []string{"dev"}, sales, false},
		{"unlabeled note is dev-only, dev caller ok", nil, dev, true},
		{"unlabeled note is dev-only, sales caller denied", nil, sales, false},
		{"empty-string scope is dev-only fail-safe", []string{"", "  "}, sales, false},
		{"multi-scope note, caller has one", []string{"dev", "sales"}, sales, true},
	}
	for _, tc := range cases {
		if got := ScopeAllows(tc.scopes, tc.allowed); got != tc.want {
			t.Errorf("%s: ScopeAllows(%v, %v) = %v, want %v", tc.name, tc.scopes, tc.allowed, got, tc.want)
		}
	}

	// CSV form (the shape the index stores) must agree with the slice form.
	if !ScopeAllowsCSV("dev,sales", sales) {
		t.Error("ScopeAllowsCSV should allow a sales caller on a dev,sales note")
	}
	if ScopeAllowsCSV("dev", sales) {
		t.Error("ScopeAllowsCSV should deny a sales caller on a dev-only note")
	}
	if !ScopeAllowsCSV("", nil) {
		t.Error("ScopeAllowsCSV with nil allowed is unrestricted")
	}
	if ScopeAllowsCSV("", sales) {
		t.Error("ScopeAllowsCSV empty (dev fail-safe) should deny a sales caller")
	}
}
