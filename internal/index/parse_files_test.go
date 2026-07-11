// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// The parallel parser must produce byte-identical graph output and identical
// issue ordering no matter how many workers run. This is the guarantee that
// makes the goroutines safe to use.
func TestParseFilesDeterministicAcrossWorkers(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"a.md": "---\nid: a\ntype: note\nwhen: 2026-01-01\n---\n# A\nlinks [[b]] and [[missing]] #x\n",
		"b.md": "---\nid: b\ntype: note\nwhen: 2026-01-01\n---\n# B\nlinks [[c]] #y\n",
		"c.md": "---\nid: c\ntype: note\nwhen: 2026-01-01\n---\n# C\n",
		"d.md": "# D no frontmatter\n[[a]]\n",
	}
	var paths []string
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)

	build := func(workers int) (int, int, []Issue) {
		notes, ferrs := ParseFiles(paths, workers)
		if len(ferrs) != 0 {
			t.Fatalf("workers=%d unexpected parse errors: %v", workers, ferrs)
		}
		g, issues := BuildGraph(notes)
		return g.NodeCount(), g.EdgeCount(), issues
	}

	n1, e1, i1 := build(1)
	n8, e8, i8 := build(8)

	if n1 != n8 || e1 != e8 {
		t.Fatalf("counts differ: workers=1 (%d nodes, %d edges) vs workers=8 (%d, %d)", n1, e1, n8, e8)
	}
	if len(i1) != len(i8) {
		t.Fatalf("issue count differs: %d vs %d", len(i1), len(i8))
	}
	for k := range i1 {
		if i1[k] != i8[k] {
			t.Fatalf("issue order differs at %d: %+v vs %+v", k, i1[k], i8[k])
		}
	}
}
