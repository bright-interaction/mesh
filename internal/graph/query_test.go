// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package graph

import "testing"

func note(id, label string, attrs map[string]any) *Node {
	return &Node{ID: "note:" + id, Kind: "note", Label: label, NoteID: id, Attrs: attrs}
}

func TestRankerScoresAndTitleWeighting(t *testing.T) {
	g := New()
	g.AddNode(note("a", "Sqlite storage decision", map[string]any{"why": "we use modernc sqlite"}))
	g.AddNode(note("b", "Team sync", map[string]any{"why": "defer sync to syncthing"}))
	g.AddNode(note("c", "Unrelated", map[string]any{"why": "nothing here"}))

	r := g.NewRanker()
	hits := r.Score("sqlite", 10)
	if len(hits) == 0 || hits[0].Node.ID != "note:a" {
		t.Fatalf("expected note:a top for 'sqlite', got %+v", hits)
	}

	// A title match should outrank an attrs-only match (labelWeight).
	g2 := New()
	g2.AddNode(note("title", "modernc sqlite", nil))
	g2.AddNode(note("body", "other", map[string]any{"why": "modernc sqlite mentioned once in prose"}))
	r2 := g2.NewRanker()
	h2 := r2.Score("modernc sqlite", 10)
	if len(h2) < 2 || h2[0].Node.ID != "note:title" {
		t.Fatalf("title match should rank first, got %+v", h2)
	}
}

func TestTokenizeStopwordsAndUnicode(t *testing.T) {
	got := Tokenize("The modernc-sqlite, och det är på GO 1.26!")
	want := map[string]bool{"modernc": true, "sqlite": true, "go": true, "26": true}
	for _, tok := range got {
		if stopwords[tok] {
			t.Errorf("stopword leaked: %q", tok)
		}
		delete(want, tok)
	}
	if len(want) != 0 {
		t.Errorf("missing expected tokens: %v (got %v)", want, got)
	}
}
