// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package index

import "testing"

func TestSearchRanksAndResolvesPath(t *testing.T) {
	dir := t.TempDir()
	a, _ := Parse("a.md", []byte("---\nid: a\ntype: note\nwhen: 2026-01-01\n---\n# Sqlite decision\nWe use modernc sqlite with flat cosine vectors.\n"))
	b, _ := Parse("b.md", []byte("---\nid: b\ntype: note\nwhen: 2026-01-01\n---\n# Sync\nDefer team sync to Syncthing.\n"))
	notes := []*ParsedNote{a, b}
	g, _ := BuildGraph(notes)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.IndexVault(notes, g); err != nil {
		t.Fatal(err)
	}

	hits, err := s.Search("modernc sqlite", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected a hit for 'modernc sqlite'")
	}
	if hits[0].NodeID != "note:a" {
		t.Errorf("top hit = %s, want note:a", hits[0].NodeID)
	}
	if hits[0].Path != "a.md" {
		t.Errorf("path = %q, want a.md", hits[0].Path)
	}
	if hits[0].Snippet == "" {
		t.Error("expected a snippet excerpt")
	}
}

func TestSearchSanitizesReservedSyntax(t *testing.T) {
	dir := t.TempDir()
	a, _ := Parse("a.md", []byte("---\nid: a\ntype: note\nwhen: 2026-01-01\n---\n# A\nhello world\n"))
	g, _ := BuildGraph([]*ParsedNote{a})
	s, _ := Open(dir)
	defer s.Close()
	s.IndexVault([]*ParsedNote{a}, g)

	// Reserved FTS5 grammar must be neutralized, not error.
	if _, err := s.Search("NEAR(* OR )", 5); err != nil {
		t.Errorf("reserved syntax should be sanitized, got error: %v", err)
	}
	if hits, _ := s.Search("nonexistentterm", 10); len(hits) != 0 {
		t.Errorf("expected no matches, got %d", len(hits))
	}
}
