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
