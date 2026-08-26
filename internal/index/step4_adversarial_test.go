// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func issueKinds(issues []Issue) []string {
	out := make([]string, len(issues))
	for i, issue := range issues {
		out[i] = issue.Kind
	}
	return out
}

func TestHeadingAnchorsKeepVisibleInlineCode(t *testing.T) {
	pn := parse(t, "headings.md", "## Use `mesh index`\n## `API`\n## Ignore `[[not-a-link]]` here\n")
	if got := headingTexts(pn); !slices.Equal(got, []string{"Use `mesh index`", "`API`", "Ignore `[[not-a-link]]` here"}) {
		t.Fatalf("headings = %q, visible code text was erased", got)
	}
	gotAnchors := make([]string, len(pn.Headings))
	for i, h := range pn.Headings {
		gotAnchors[i] = h.Anchor
	}
	if !slices.Equal(gotAnchors, []string{"use-mesh-index", "api", "ignore-not-a-link-here"}) {
		t.Fatalf("anchors = %q, want visible code text represented", gotAnchors)
	}
	if got := linkTargets(pn); len(got) != 0 {
		t.Fatalf("wikilink syntax inside the heading's code span became links: %v", got)
	}
}

func TestStableIDStillResolvesAfterBasenameRename(t *testing.T) {
	source := parse(t, "source.md", "---\nid: source\ntype: note\nwhen: 2026-01-01\n---\n# Source\n[[target]]\n")
	renamed := parse(t, "renamed.md", "---\nid: target\ntype: note\nwhen: 2026-01-01\n---\n# Target\n")

	g, issues := BuildGraph([]*ParsedNote{source, renamed})
	if !hasEdge(g.Neighbors("note:source"), "note:target", "references") {
		t.Fatalf("stable id did not preserve source -> target after the target basename changed; issues=%+v", issues)
	}
	for _, issue := range issues {
		if issue.Kind == "broken-link" {
			t.Fatalf("stable-id link was reported broken after rename: %+v", issue)
		}
	}
}

func TestBasenameCannotSilentlyHijackARenamedStableID(t *testing.T) {
	source := parse(t, "source.md", "---\nid: source\ntype: note\nwhen: 2026-01-01\n---\n# Source\n[[target]]\n")
	renamedOwner := parse(t, "renamed.md", "---\nid: target\ntype: note\nwhen: 2026-01-01\n---\n# Original target\n")
	newBasename := parse(t, "target.md", "---\nid: newcomer\ntype: note\nwhen: 2026-01-01\n---\n# New target filename\n")

	for _, notes := range [][]*ParsedNote{{source, renamedOwner, newBasename}, {source, newBasename, renamedOwner}} {
		g, issues := BuildGraph(notes)
		for _, target := range []string{"note:target", "note:newcomer"} {
			if hasEdge(g.Neighbors("note:source"), target, "references") {
				t.Fatalf("cross-namespace collision silently resolved to %s; issues=%+v", target, issues)
			}
		}
		if !slices.Contains(issueKinds(issues), "ambiguous-link") {
			t.Fatalf("basename/stable-id collision was not diagnosed: %+v", issues)
		}
	}
}

func TestReservedAnchorSeparatorCannotBecomeANoteID(t *testing.T) {
	_, err := Parse("collision.md", []byte("---\nid: owner#section\ntype: note\nwhen: 2026-01-01\n---\n# Collision\n"))
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("Parse accepted id containing graph anchor separator '#': %v", err)
	}
}

func TestDuplicateHeadingAnchorIsReported(t *testing.T) {
	pn := parse(t, "duplicate.md", "---\nid: duplicate\ntype: note\nwhen: 2026-01-01\n---\n# Duplicate\n## Repeat\none\n## Repeat\ntwo\n")
	_, issues := BuildGraph([]*ParsedNote{pn})
	if !slices.Contains(issueKinds(issues), "duplicate-anchor") {
		t.Fatalf("duplicate headings silently collapsed; issues=%+v", issues)
	}
}

func TestAnchoredWikilinksValidateExternalAndLocalHeadings(t *testing.T) {
	target := parse(t, "notes/target.md", "---\nid: target\ntype: note\nwhen: 2026-01-01\n---\n# Target\n## Overview\n")
	source := parse(t, "source.md", "---\nid: source\ntype: note\nwhen: 2026-01-01\n---\n# Source\n## Local\n[[notes/target#Overview|valid]] [[target#Missing|bad]] [[#Local]] [[#Absent]]\n")

	g, issues := BuildGraph([]*ParsedNote{source, target})
	if !hasEdge(g.Neighbors("note:source"), "note:target", "references") {
		t.Fatalf("valid path-qualified aliased anchor did not resolve; issues=%+v", issues)
	}
	var broken []string
	for _, issue := range issues {
		if issue.Kind == "broken-anchor" {
			broken = append(broken, issue.Msg)
		}
	}
	joined := strings.Join(broken, "\n")
	if len(broken) != 2 || !strings.Contains(joined, "Missing") || !strings.Contains(joined, "Absent") {
		t.Fatalf("broken anchors = %v, want external Missing and local Absent only", broken)
	}
}

func TestUnicodeEquivalentWikilinkPathsResolve(t *testing.T) {
	target := parse(t, "notes/Åtgärd.md", "---\nid: unicode-target\ntype: note\nwhen: 2026-01-01\n---\n# Åtgärd\n")
	source := parse(t, "source.md", "---\nid: source\ntype: note\nwhen: 2026-01-01\n---\n# Source\n[[notes/A\u030Atga\u0308rd]]\n")
	g, issues := BuildGraph([]*ParsedNote{source, target})
	if !hasEdge(g.Neighbors("note:source"), "note:unicode-target", "references") {
		t.Fatalf("NFC filename and NFD path-qualified link did not meet; issues=%+v", issues)
	}
}

func TestNormalizedPathCollisionNeverResolvesArbitrarily(t *testing.T) {
	first := parse(t, "notes/Åtgärd.md", "---\nid: first\ntype: note\nwhen: 2026-01-01\n---\n# First\n")
	second := parse(t, "notes/A\u030Atga\u0308rd.md", "---\nid: second\ntype: note\nwhen: 2026-01-01\n---\n# Second\n")
	source := parse(t, "source.md", "---\nid: source\ntype: note\nwhen: 2026-01-01\n---\n# Source\n[[notes/Åtgärd]]\n")

	for _, notes := range [][]*ParsedNote{{source, first, second}, {source, second, first}} {
		g, issues := BuildGraph(notes)
		for _, target := range []string{"note:first", "note:second"} {
			if hasEdge(g.Neighbors("note:source"), target, "references") {
				t.Fatalf("normalized path collision resolved arbitrarily to %s; issues=%+v", target, issues)
			}
		}
		kinds := issueKinds(issues)
		if !slices.Contains(kinds, "ambiguous-path-key") || !slices.Contains(kinds, "ambiguous-link") {
			t.Fatalf("normalized path collision was not diagnosed: %+v", issues)
		}
	}
}

func TestFrontmatterProseCommentsAgreeAcrossGraphAndSearch(t *testing.T) {
	pn := parse(t, "frontmatter.md", "---\nid: fm\ntype: gotcha\nwhen: 2026-01-01\ndo: '<!-- secretneedle [[target]]'\ndont: safe\nwhy: safe\n---\n# Frontmatter\n")
	if !slices.Contains(issueKinds(pn.Issues), "unterminated-comment") {
		t.Fatalf("unterminated comment in do: was not surfaced: %+v", pn.Issues)
	}
	if got := searchText(pn); strings.Contains(got, "secretneedle") || strings.Contains(got, "target") {
		t.Fatalf("search indexed frontmatter prose the graph treats as commented out: %q", got)
	}
}

func TestFrontmatterCommentsDoNotLeakIntoGraphRanking(t *testing.T) {
	pn := parse(t, "frontmatter-rank.md", "---\nid: fm-rank\ntype: gotcha\nwhen: 2026-01-01\ntitle: Plain title\ndo: '<!-- secretneedle -->'\ndont: safe\nwhy: visibleguidance\n---\n# Plain heading\n")
	g, issues := BuildGraph([]*ParsedNote{pn})
	if len(issues) != 0 {
		t.Fatalf("graph issues: %+v", issues)
	}
	if hits := g.NewRanker().Score("secretneedle", 10); len(hits) != 0 {
		t.Fatalf("graph BM25 indexed commented frontmatter prose: %+v", hits)
	}
	if hits := g.NewRanker().Score("visibleguidance", 10); len(hits) != 1 || hits[0].Node.ID != "note:fm-rank" {
		t.Fatalf("visible frontmatter prose stopped ranking: %+v", hits)
	}
}

func TestBasenameRenameKeepsEdgeInIncrementalAndFullIndexes(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "source.md", "---\nid: source\ntype: note\nwhen: 2026-01-01\n---\n# Source\n[[target]]\n")
	write(t, dir, "target.md", "---\nid: target\ntype: note\nwhen: 2026-01-01\n---\n# Target\n")
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	live := NewLiveIndexer(store, dir)
	if _, err := live.Reconcile(true); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "target.md"), filepath.Join(dir, "renamed.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := live.Reconcile(true); err != nil {
		t.Fatal(err)
	}
	assertStableEdge := func(stage string) {
		t.Helper()
		var path string
		if err := store.readDB.QueryRow(`SELECT path FROM notes WHERE id='target'`).Scan(&path); err != nil {
			t.Fatal(err)
		}
		if path != "renamed.md" {
			t.Errorf("%s path = %q, want renamed.md", stage, path)
		}
		var edges int
		if err := store.readDB.QueryRow(`SELECT count(*) FROM edges WHERE source='note:source' AND target='note:target' AND relation='references'`).Scan(&edges); err != nil {
			t.Fatal(err)
		}
		if edges != 1 {
			t.Errorf("%s reference edge count = %d, want 1", stage, edges)
		}
	}
	assertStableEdge("incremental")
	if _, err := Reindex(store, dir); err != nil {
		t.Fatal(err)
	}
	assertStableEdge("full")
}
