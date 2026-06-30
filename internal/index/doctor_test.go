// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"os"
	"path/filepath"
	"testing"
)

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestDriftReport(t *testing.T) {
	dir := t.TempDir()
	wr := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wr("a.md", "---\nid: a\ntype: note\nwhen: 2026-01-01\n---\n# A\noriginal body\n")
	wr("b.md", "---\nid: b\ntype: note\nwhen: 2026-01-01\n---\n# B\nbody\n")

	a, _ := ParseFile(filepath.Join(dir, "a.md"))
	a.Path = "a.md"
	b, _ := ParseFile(filepath.Join(dir, "b.md"))
	b.Path = "b.md"
	g, _ := BuildGraph([]*ParsedNote{a, b})
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.IndexVault([]*ParsedNote{a, b}, g); err != nil {
		t.Fatal(err)
	}

	if d, _ := s.DriftReport(dir); d.Any() {
		t.Fatalf("fresh index should have no drift, got %+v", d)
	}

	wr("c.md", "---\nid: c\ntype: note\nwhen: 2026-01-01\n---\n# C\nnew\n")
	wr("a.md", "---\nid: a\ntype: note\nwhen: 2026-01-01\n---\n# A\nCHANGED body now\n")
	if err := os.Remove(filepath.Join(dir, "b.md")); err != nil {
		t.Fatal(err)
	}

	d, err := s.DriftReport(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(d.Added, "c.md") {
		t.Errorf("expected c.md in Added, got %v", d.Added)
	}
	if !contains(d.Changed, "a.md") {
		t.Errorf("expected a.md in Changed, got %v", d.Changed)
	}
	if !contains(d.Removed, "b.md") {
		t.Errorf("expected b.md in Removed, got %v", d.Removed)
	}
}
