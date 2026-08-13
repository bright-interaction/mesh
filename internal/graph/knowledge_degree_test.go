// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package graph

import "testing"

// A note's connectedness must be its LINKS, not its length. Raw Degree counts every
// edge on the node, and a note owns one "contains" edge per heading plus one "tagged"
// edge per tag, so the longest note in a vault outranked every real hub: the printed
// "most-connected notes" list led with a note that had no links at all.
func TestKnowledgeDegreeCountsOnlyNoteToNoteReferences(t *testing.T) {
	g := New()
	addNote := func(id string) {
		g.AddNode(&Node{ID: NotePrefix + id, Kind: "note", Label: id, NoteID: id, NotePath: id + ".md"})
	}
	addNote("longnote")
	addNote("hub")

	// longnote: 30 headings, one tag, zero links.
	for i := 0; i < 30; i++ {
		anchor := string(rune('a'+i%26)) + string(rune('0'+i/26))
		hid := NotePrefix + "longnote#" + anchor
		g.AddNode(&Node{ID: hid, Kind: "heading", Label: anchor, NoteID: "longnote", Anchor: anchor})
		g.AddEdge(Edge{Source: NotePrefix + "longnote", Target: hid, Relation: "contains"})
	}
	g.AddNode(&Node{ID: "tag:log", Kind: "tag", Label: "log"})
	g.AddEdge(Edge{Source: NotePrefix + "longnote", Target: "tag:log", Relation: "tagged"})

	// hub: linked both ways by 12 notes, and one heading of its own.
	hHead := NotePrefix + "hub#intro"
	g.AddNode(&Node{ID: hHead, Kind: "heading", Label: "Intro", NoteID: "hub", Anchor: "intro"})
	g.AddEdge(Edge{Source: NotePrefix + "hub", Target: hHead, Relation: "contains"})
	for i := 0; i < 12; i++ {
		id := "n" + string(rune('a'+i))
		addNote(id)
		g.AddEdge(Edge{Source: NotePrefix + "hub", Target: NotePrefix + id, Relation: RelReferences})
		g.AddEdge(Edge{Source: NotePrefix + id, Target: NotePrefix + "hub", Relation: RelReferences})
	}

	g.RecomputeDegrees()

	long, _ := g.Node(NotePrefix + "longnote")
	hub, _ := g.Node(NotePrefix + "hub")

	if long.Degree <= hub.Degree {
		t.Fatalf("fixture is wrong: raw degree should favour the long note (long=%d hub=%d)", long.Degree, hub.Degree)
	}
	if long.KnowledgeDegree != 0 {
		t.Errorf("a note with 30 headings and no links has 0 knowledge connections, got %d", long.KnowledgeDegree)
	}
	if hub.KnowledgeDegree != 12 {
		t.Errorf("hub links 12 distinct notes both ways, so knowledge degree = 12, got %d", hub.KnowledgeDegree)
	}
	if hub.KnowledgeDegree <= long.KnowledgeDegree {
		t.Errorf("the linked hub must outrank the long unlinked note: hub=%d long=%d", hub.KnowledgeDegree, long.KnowledgeDegree)
	}

	// A leaf sees the hub once, not once per direction.
	leaf, _ := g.Node(NotePrefix + "na")
	if leaf.KnowledgeDegree != 1 {
		t.Errorf("a leaf linked to and from the hub knows 1 note, got %d", leaf.KnowledgeDegree)
	}

	// Scaffolding nodes are not knowledge.
	head, _ := g.Node(hHead)
	if head.KnowledgeDegree != 0 {
		t.Errorf("a heading node has no knowledge connections, got %d", head.KnowledgeDegree)
	}
}

// The two graph build paths must agree on both degrees, not just the raw one.
func TestRecomputeDegreesIsOrderIndependent(t *testing.T) {
	build := func(nodesFirst bool) *Graph {
		g := New()
		add := func(id string) {
			g.AddNode(&Node{ID: NotePrefix + id, Kind: "note", NoteID: id})
		}
		if nodesFirst {
			add("a")
			add("b")
			g.AddEdge(Edge{Source: NotePrefix + "a", Target: NotePrefix + "b", Relation: RelReferences})
		} else {
			add("a")
			g.AddEdge(Edge{Source: NotePrefix + "a", Target: NotePrefix + "b", Relation: RelReferences})
			add("b")
		}
		g.RecomputeDegrees()
		return g
	}
	for _, id := range []string{NotePrefix + "a", NotePrefix + "b"} {
		x, _ := build(true).Node(id)
		y, _ := build(false).Node(id)
		if x.Degree != y.Degree || x.KnowledgeDegree != y.KnowledgeDegree {
			t.Fatalf("%s differs by build order: nodes-first %d/%d, interleaved %d/%d",
				id, x.Degree, x.KnowledgeDegree, y.Degree, y.KnowledgeDegree)
		}
		if x.KnowledgeDegree != 1 {
			t.Fatalf("%s knows exactly one note, got %d", id, x.KnowledgeDegree)
		}
	}
}
