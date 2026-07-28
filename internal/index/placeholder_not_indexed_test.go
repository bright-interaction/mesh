// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/vault"
)

// `mesh new` writes the literal "TODO" into do/dont/why so a human editing the file can see
// what is missing. That is useful in the file and actively harmful in the index: 77 notes
// in one vault matched a search for "TODO" with their own placeholder as the excerpt, and
// the embedding header (prepended to EVERY chunk of a note) read "TODO\nTODO\nTODO", which
// pulls unfilled notes toward each other in vector space on a token that means nothing.
//
// The placeholder stays in the file. It must not reach retrieval.
func TestPlaceholderFlywheelFieldsAreNotIndexed(t *testing.T) {
	scaffolded := &ParsedNote{
		Path: "gotchas/x.md",
		Body: "# X\n\nReal body text about widgets.\n",
		FM: &vault.Frontmatter{
			ID: "x", Type: "gotcha", Title: "X",
			Do: "TODO", Dont: "TODO", Why: "TODO",
		},
	}

	if got := searchText(scaffolded); strings.Contains(strings.ToUpper(got), "TODO") {
		t.Errorf("a placeholder reached the FTS text, so the note matches a search for TODO:\n%s", got)
	}
	for _, c := range ChunkText(scaffolded) {
		if strings.Contains(strings.ToUpper(c), "TODO") {
			t.Errorf("a placeholder reached an embedding chunk header:\n%s", c)
		}
	}

	// The real content must still be indexed, or this "fix" would just delete signal.
	if got := searchText(scaffolded); !strings.Contains(got, "widgets") {
		t.Errorf("the body stopped being indexed:\n%s", got)
	}
}

// Authored guidance is the whole point of these fields: it is prepended to every embedding
// chunk precisely so a note retrieves on what it advises, not only on its prose. Filtering
// placeholders must not touch it.
func TestAuthoredFlywheelFieldsStillIndexed(t *testing.T) {
	authored := &ParsedNote{
		Path: "gotchas/y.md",
		Body: "# Y\n\nBody.\n",
		FM: &vault.Frontmatter{
			ID: "y", Type: "gotcha", Title: "Y",
			Do:   "Always close the result set.",
			Dont: "Never retry through a held lock.",
			Why:  "A leaked Rows pins the WAL.",
		},
	}

	txt := searchText(authored)
	for _, want := range []string{"close the result set", "retry through a held lock", "pins the WAL"} {
		if !strings.Contains(txt, want) {
			t.Errorf("authored guidance %q missing from the FTS text", want)
		}
	}
	chunks := ChunkText(authored)
	if len(chunks) == 0 {
		t.Fatal("no chunks produced")
	}
	if !strings.Contains(chunks[0], "close the result set") {
		t.Errorf("authored guidance missing from the embedding header:\n%s", chunks[0])
	}
}

// The indexer and lint must agree on what "unfilled" means, or a field can be nagged about
// and indexed at the same time, which is how this survived.
func TestUnfilledMatchesWhatLintConsidersUnfilled(t *testing.T) {
	unfilled := []string{"", "   ", "TODO", "todo", "TODO: fill this in", "  TODO  "}
	for _, s := range unfilled {
		if !vault.Unfilled(s) {
			t.Errorf("vault.Unfilled(%q) = false, but lint reports this field as not filled", s)
		}
	}
	// The word-boundary cases matter most: a false positive here would silently drop real
	// authored guidance out of search now that the indexer shares this predicate.
	filled := []string{
		"Always close the result set.",
		"Do not use TODO lists here", // mentions TODO, does not start with it
		"todos are tracked in the tracker",
		"TODOs belong in the issue tracker, not here",
	}
	for _, s := range filled {
		if vault.Unfilled(s) {
			t.Errorf("vault.Unfilled(%q) = true, but this is authored guidance", s)
		}
	}
}
