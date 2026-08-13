// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestBuildExcerptMatchesTheOldSnippetShape pins the excerpt format now that Mesh
// produces it instead of FTS5. A card's snippet is what an agent reads before
// deciding whether to fetch a note, so the window has to stay centred on the match,
// the match has to stay marked, and cut edges have to stay marked as cuts.
func TestBuildExcerptMatchesTheOldSnippetShape(t *testing.T) {
	const body = "The deploy pipeline runs nightly and the rollback window is short. " +
		"We keep the vault index warm so retrieval stays cheap for every agent that asks. " +
		"Bilingual text like atervinning has to survive the cut."

	cases := []struct {
		name         string
		terms        []string
		wantContains []string
		wantPrefix   string
	}{
		{
			name:         "the match is bracketed and the window is centred on it",
			terms:        []string{"rollback"},
			wantContains: []string{"[rollback]", excerptEllipsis},
			wantPrefix:   excerptEllipsis, // text was cut before the match
		},
		{
			name:         "a match near the end still yields a window around it",
			terms:        []string{"atervinning"},
			wantContains: []string{"[atervinning]"},
			wantPrefix:   excerptEllipsis,
		},
		{
			name:         "no match in the text falls back to the head, as snippet() did",
			terms:        []string{"nothinghere"},
			wantContains: []string{"The deploy pipeline"},
			wantPrefix:   "The",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildExcerpt(body, tc.terms, searchExcerptTokens)
			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("excerpt %q does not contain %q", got, want)
				}
			}
			if !strings.HasPrefix(got, tc.wantPrefix) {
				t.Errorf("excerpt %q does not start with %q", got, tc.wantPrefix)
			}
			if !utf8.ValidString(got) {
				t.Errorf("excerpt %q is not valid UTF-8: the window must land on token boundaries", got)
			}
		})
	}

	t.Run("a term is matched as a whole token, not as a substring", func(t *testing.T) {
		got := buildExcerpt("deployment of the deploy pipeline", []string{"deploy"}, searchExcerptTokens)
		if strings.Contains(got, "[deploy]ment") {
			t.Errorf("excerpt %q bracketed a substring; FTS5 matches tokens, and so must the excerpt", got)
		}
		if !strings.Contains(got, "[deploy]") {
			t.Errorf("excerpt %q did not bracket the whole-token match", got)
		}
	})

	t.Run("empty inputs are safe", func(t *testing.T) {
		if got := buildExcerpt("", []string{"x"}, searchExcerptTokens); got != "" {
			t.Errorf("excerpt of empty text = %q, want empty", got)
		}
		if got := buildExcerpt("some text", nil, searchExcerptTokens); got == "" {
			t.Error("a hit with no usable terms must still get the head of the text as its excerpt")
		}
	})
}
