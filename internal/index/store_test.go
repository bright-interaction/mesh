// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import "testing"

func TestStoreIndexVaultRoundTrip(t *testing.T) {
	dir := t.TempDir()
	a, err := Parse("a.md", []byte("---\nid: a\ntype: note\nwhen: 2026-01-01\n---\n# A\nlinks [[b]] #x\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse("b.md", []byte("---\nid: b\ntype: note\nwhen: 2026-01-01\n---\n# B\nbody about widgets\n"))
	if err != nil {
		t.Fatal(err)
	}
	notes := []*ParsedNote{a, b}
	g, _ := BuildGraph(notes)

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	n, err := s.IndexVault(notes, g)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if n != 2 {
		t.Fatalf("wrote %d notes, want 2", n)
	}

	assertCount(t, s, "notes", 2)
	assertCount(t, s, "nodes", g.NodeCount())
	assertCount(t, s, "edges", g.EdgeCount())

	// Re-index must be idempotent (full wipe + insert).
	if _, err := s.IndexVault(notes, g); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	assertCount(t, s, "notes", 2)
	assertCount(t, s, "edges", g.EdgeCount())

	// FTS5 must be compiled in and queryable.
	var hits int
	if err := s.readDB.QueryRow(`SELECT count(*) FROM search_index WHERE search_index MATCH 'widgets'`).Scan(&hits); err != nil {
		t.Fatalf("fts query: %v", err)
	}
	if hits != 1 {
		t.Errorf("fts 'widgets' hits = %d, want 1", hits)
	}
}

func assertCount(t *testing.T, s *Store, table string, want int) {
	t.Helper()
	got, err := s.Count(table)
	if err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	if got != want {
		t.Errorf("%s count = %d, want %d", table, got, want)
	}
}
