// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package tokenize

import "testing"

// TestCountKnownCl100k locks in known cl100k_base counts. These are exact for the
// GPT-4 BPE; if the codec is ever swapped (e.g. to o200k) these change, so this is
// the guard that we are counting with the tokenizer we documented.
func TestCountKnownCl100k(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"hello world", 2},
		{"The quick brown fox jumps over the lazy dog.", 10},
		{"## Decision\nUse modernc.org/sqlite (no cgo) for the index.", 18},
	}
	for _, tc := range cases {
		if got := Count(tc.in); got != tc.want {
			t.Errorf("Count(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestCountIsRealBPENotHeuristic proves the real tokenizer is active: on dense
// code the heuristic over-counts substantially, so a real BPE must differ.
func TestCountIsRealBPENotHeuristic(t *testing.T) {
	code := "func (s *Store) IndexVaultIncremental(u []*ParsedNote) error { return nil }"
	got, h := Count(code), heuristic(code)
	if got == h {
		t.Fatalf("Count == heuristic (%d): the real BPE does not appear to be active", got)
	}
	if got <= 0 {
		t.Fatalf("Count returned %d", got)
	}
	// The heuristic notably over-counts code; the BPE should be lower here.
	if got >= h {
		t.Errorf("expected BPE count (%d) below the heuristic (%d) on dense code", got, h)
	}
}

func TestHeuristicFallback(t *testing.T) {
	if heuristic("") != 0 {
		t.Error("empty heuristic should be 0")
	}
	// One four-letter word -> 1 token; a trailing symbol -> +1.
	if got := heuristic("word!"); got != 2 {
		t.Errorf("heuristic(%q) = %d, want 2", "word!", got)
	}
}
