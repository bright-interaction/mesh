// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package graph

import "testing"

// Two disconnected triangles must fall into two communities. (Label propagation
// can merge bridge-connected clusters; that quality gap is what the later
// Louvain upgrade addresses. LP reliably separates disconnected components.)
func TestDetectCommunitiesTwoClusters(t *testing.T) {
	g := New()
	for _, id := range []string{"a", "b", "c", "x", "y", "z"} {
		g.AddNode(&Node{ID: id, Kind: "note", Label: id})
	}
	edge := func(s, d string) { g.AddEdge(Edge{Source: s, Target: d, Relation: "references", Weight: 1}) }
	// cluster 1: a-b-c
	edge("a", "b")
	edge("b", "c")
	edge("c", "a")
	// cluster 2: x-y-z (disconnected from cluster 1)
	edge("x", "y")
	edge("y", "z")
	edge("z", "x")

	k := g.DetectCommunities(0)
	if k != 2 {
		t.Fatalf("expected 2 communities, got %d", k)
	}
	ca, _ := g.Node("a")
	cb, _ := g.Node("b")
	cz, _ := g.Node("z")
	if ca.Community != cb.Community {
		t.Errorf("a and b should share a community: %d vs %d", ca.Community, cb.Community)
	}
	if ca.Community == cz.Community {
		t.Errorf("a and z should be in different communities")
	}
	// renumbering is smallest-member-first: a's cluster is community 0
	if ca.Community != 0 {
		t.Errorf("a's community should be 0 (smallest member), got %d", ca.Community)
	}
}

// clique wires every pair among ids with a weight-1 edge.
func clique(g *Graph, ids ...string) {
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			g.AddEdge(Edge{Source: ids[i], Target: ids[j], Relation: "references", Weight: 1})
		}
	}
}

// TestLouvainSeparatesBridgedCliques is the headline quality test: two 4-cliques
// joined by a single bridge edge are one connected component, but Louvain keeps
// them as two communities (the dense intra-clique structure dominates the one
// bridge). It also checks Louvain's modularity is at least label propagation's on
// the same graph, proving the upgrade is real, not a regression.
func TestLouvainSeparatesBridgedCliques(t *testing.T) {
	g := New()
	left := []string{"a", "b", "c", "d"}
	right := []string{"p", "q", "r", "s"}
	for _, id := range append(append([]string{}, left...), right...) {
		g.AddNode(&Node{ID: id, Kind: "note", Label: id})
	}
	clique(g, left...)
	clique(g, right...)
	g.AddEdge(Edge{Source: "d", Target: "p", Relation: "references", Weight: 1}) // the single bridge

	k := g.DetectCommunities(0)
	if k != 2 {
		t.Fatalf("two bridged cliques should be 2 communities, got %d", k)
	}
	ca, _ := g.Node("a")
	cd, _ := g.Node("d")
	cp, _ := g.Node("p")
	if ca.Community != cd.Community {
		t.Errorf("the left clique must share a community: a=%d d=%d", ca.Community, cd.Community)
	}
	if ca.Community == cp.Community {
		t.Errorf("the two cliques must be different communities: a=%d p=%d", ca.Community, cp.Community)
	}

	// Louvain modularity >= label propagation modularity on the same adjacency.
	ids := []string{"a", "b", "c", "d", "p", "q", "r", "s"}
	sortStrings(ids)
	a := g.buildAdjacency(ids)
	qL := a.modularity(a.louvain(20))
	qLP := a.modularity(a.labelProp(20))
	if qL < qLP-1e-9 {
		t.Errorf("Louvain modularity (%.4f) should be >= label-prop (%.4f)", qL, qLP)
	}
}

// TestHeadingsInheritParentCommunity: a heading node is clustered with its parent
// note, never split into its own community.
func TestHeadingsInheritParentCommunity(t *testing.T) {
	g := New()
	g.AddNode(&Node{ID: "note:a", Kind: "note", Label: "A", NoteID: "a"})
	g.AddNode(&Node{ID: "note:b", Kind: "note", Label: "B", NoteID: "b"})
	g.AddNode(&Node{ID: "note:a#intro", Kind: "heading", Label: "Intro", NoteID: "a"})
	g.AddEdge(Edge{Source: "note:a", Target: "note:b", Relation: "references", Weight: 1})
	g.AddEdge(Edge{Source: "note:a", Target: "note:a#intro", Relation: "contains", Weight: 1})

	g.DetectCommunities(0)
	na, _ := g.Node("note:a")
	nh, _ := g.Node("note:a#intro")
	if na.Community != nh.Community {
		t.Errorf("heading should inherit its parent note's community: note=%d heading=%d", na.Community, nh.Community)
	}
}

// TestOrphanHeadingDeterministic: two orphan headings (parent note absent) must
// get stable community ids across repeated runs, not flip with map iteration order.
func TestOrphanHeadingDeterministic(t *testing.T) {
	build := func() *Graph {
		g := New()
		g.AddNode(&Node{ID: "note:a", Kind: "note", Label: "A", NoteID: "a"})
		// Headings whose parent notes are NOT in the graph (orphans).
		g.AddNode(&Node{ID: "note:g1#h", Kind: "heading", Label: "H1", NoteID: "g1"})
		g.AddNode(&Node{ID: "note:g2#h", Kind: "heading", Label: "H2", NoteID: "g2"})
		return g
	}
	first := build()
	first.DetectCommunities(0)
	want1, _ := first.Node("note:g1#h")
	want2, _ := first.Node("note:g2#h")
	for i := 0; i < 50; i++ {
		g := build()
		g.DetectCommunities(0)
		h1, _ := g.Node("note:g1#h")
		h2, _ := g.Node("note:g2#h")
		if h1.Community != want1.Community || h2.Community != want2.Community {
			t.Fatalf("orphan heading community ids must be stable: got g1=%d g2=%d want g1=%d g2=%d",
				h1.Community, h2.Community, want1.Community, want2.Community)
		}
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func TestDetectCommunitiesDeterministic(t *testing.T) {
	build := func() *Graph {
		g := New()
		for _, id := range []string{"a", "b", "c", "d"} {
			g.AddNode(&Node{ID: id, Kind: "note", Label: id})
		}
		g.AddEdge(Edge{Source: "a", Target: "b", Relation: "r", Weight: 1})
		g.AddEdge(Edge{Source: "c", Target: "d", Relation: "r", Weight: 1})
		return g
	}
	g1, g2 := build(), build()
	g1.DetectCommunities(0)
	g2.DetectCommunities(0)
	for _, id := range []string{"a", "b", "c", "d"} {
		n1, _ := g1.Node(id)
		n2, _ := g2.Node(id)
		if n1.Community != n2.Community {
			t.Errorf("nondeterministic community for %s: %d vs %d", id, n1.Community, n2.Community)
		}
	}
}
