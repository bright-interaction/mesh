// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
)

// `mesh orient` is what mesh_setup_hooks fires at every Claude Code SessionStart, so
// its "Entry points (most-connected notes)" list is the first thing an agent ever reads
// about a vault, and each line ends in "[N links]".
//
// The fixture is the shape that broke it: one note with 30 headings and zero links, and
// one hub that 12 notes link to and that links back to all 12. Ranked by raw fan-out the
// long note wins (its own headings are edges) and prints "[30 links]" while having none.
func TestOrientEntryPointsRankHubsByRealLinks(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var long strings.Builder
	long.WriteString("---\nid: longnote\ntype: note\nwhen: 2026-01-01\n---\n# Long Note\n")
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&long, "\n## Section %d\nbody text %d, no links at all.\n", i, i)
	}
	write("longnote.md", long.String())

	var hub strings.Builder
	hub.WriteString("---\nid: hub\ntype: note\nwhen: 2026-01-01\n---\n# Hub\n")
	for i := 1; i <= 12; i++ {
		fmt.Fprintf(&hub, "- [[leaf%02d]]\n", i)
		write(fmt.Sprintf("leaf%02d.md", i), fmt.Sprintf(
			"---\nid: leaf%02d\ntype: note\nwhen: 2026-01-01\n---\n# Leaf %d\nback to [[hub]].\n", i, i))
	}
	write("hub.md", hub.String())

	store, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, _, err := index.ReindexFull(store, dir); err != nil {
		t.Fatal(err)
	}
	g, err := store.LoadGraph()
	if err != nil {
		t.Fatal(err)
	}

	text := orientText(store, g)
	entry := text
	if i := strings.Index(text, "## Entry points"); i >= 0 {
		entry = text[i:]
	}
	if j := strings.Index(entry[1:], "\n## "); j >= 0 {
		entry = entry[:j+1]
	}

	var lines []string
	for _, l := range strings.Split(entry, "\n") {
		if strings.HasPrefix(l, "- ") {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		t.Fatalf("no entry points printed:\n%s", text)
	}
	const hubLine = "- hub (hub.md) [12 links]"
	if lines[0] != hubLine {
		t.Errorf("the first entry point must be the linked hub with its real count %q, got %q\n%s", hubLine, lines[0], entry)
	}
	for _, l := range lines {
		if strings.Contains(l, "longnote") && !strings.Contains(l, "[0 links]") {
			t.Errorf("the 30-heading zero-link note must never be printed with a link count it does not have: %q", l)
		}
	}
}
