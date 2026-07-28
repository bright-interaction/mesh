// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"testing"

	"github.com/bright-interaction/mesh/internal/vault"
)

// The title is the first thing anyone reads on a search card, agent or human. When it was
// missing, Mesh showed the raw key, so 33 notes in one live vault surfaced as
// "paas-resurrected-weekly-by-root-maintenance-cron". That is the identifier, not a
// title, and it is markedly harder to scan a result list of them.
func TestTitleFallsBackToSomethingReadable(t *testing.T) {
	cases := []struct {
		name string
		note *ParsedNote
		want string
	}{
		{
			name: "an explicit title always wins",
			note: &ParsedNote{Key: "some-key", FM: &vault.Frontmatter{ID: "some-key", Title: "A Real Title"}},
			want: "A Real Title",
		},
		{
			name: "a title that merely repeats the id is treated as absent",
			note: &ParsedNote{Key: "paas-resurrected-weekly", FM: &vault.Frontmatter{
				ID: "paas-resurrected-weekly", Title: "paas-resurrected-weekly"}},
			want: "PaaS resurrected weekly",
		},
		{
			name: "the note's own H1 is preferred over the key",
			note: &ParsedNote{
				Key:      "sms-gateway",
				FM:       &vault.Frontmatter{ID: "sms-gateway"},
				Headings: []Heading{{Level: 1, Text: "SMS Gateway"}},
			},
			want: "SMS Gateway",
		},
		{
			name: "a deeper heading is not a title",
			note: &ParsedNote{
				Key:      "some-note",
				FM:       &vault.Frontmatter{ID: "some-note"},
				Headings: []Heading{{Level: 2, Text: "Symptom"}},
			},
			want: "Some note",
		},
		{
			name: "no title and no H1 falls back to a readable key",
			note: &ParsedNote{Key: "flare-has-a-built-in-mcp", FM: &vault.Frontmatter{ID: "flare-has-a-built-in-mcp"}},
			want: "Flare has a built in mcp",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := titleOf(tc.note); got != tc.want {
				t.Errorf("titleOf() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The fallback must never invent words, only reformat the note's own. If it ever starts
// guessing (expanding acronyms, title casing), a card would assert something the author
// never wrote.
func TestHumanizeKeyOnlyReformats(t *testing.T) {
	cases := map[string]string{
		"a-b-c":       "A b c",
		"single":      "Single",
		"":            "",
		"2026-07-28":  "2026 07 28",
		"Already-Cap": "Already Cap",
	}
	for in, want := range cases {
		if got := humanizeKey(in); got != want {
			t.Errorf("humanizeKey(%q) = %q, want %q", in, got, want)
		}
	}
}
