package web

import (
	"regexp"
	"testing"

	"github.com/bright-interaction/mesh/internal/graph"
)

func linkedGraph() *graph.Graph {
	g := graph.New()
	add := func(id, label string) {
		g.AddNode(&graph.Node{ID: notePrefix + id, Kind: "note", Label: label, NoteID: id, NotePath: id + ".md", Attrs: map[string]any{"type": "note"}})
	}
	add("hub", "Hub")
	add("alpha", "Alpha")
	add("beta", "Beta")
	g.AddNode(&graph.Node{ID: "tag:core", Kind: "tag", Label: "core"})
	edge := func(s, t, rel string) { g.AddEdge(graph.Edge{Source: s, Target: t, Relation: rel}) }
	edge("note:hub", "note:alpha", "references")
	edge("note:hub", "note:beta", "references")
	edge("note:alpha", "note:beta", "references")
	edge("note:hub", "tag:core", "tagged")
	g.DetectCommunities(0)
	return g
}

func TestBuildExport(t *testing.T) {
	exp := BuildExport(linkedGraph(), "/vault")

	// Notes only (the tag node is excluded), index = highest degree.
	if exp.Meta.NodeCount != 3 || len(exp.Nodes) != 3 {
		t.Fatalf("want 3 note nodes, got %d", len(exp.Nodes))
	}
	if exp.Meta.IndexID != "hub" {
		t.Fatalf("index should be the highest-degree note (hub), got %q", exp.Meta.IndexID)
	}
	if exp.Nodes[0].ID != "hub" {
		t.Fatalf("nodes should be importance-sorted (hub first), got %q", exp.Nodes[0].ID)
	}

	// Edges are note-to-note only (no tag edges leak in).
	hex := regexp.MustCompile(`^#[0-9a-f]{6}$`)
	for _, e := range exp.Edges {
		if e.Source == "" || e.Target == "" {
			t.Fatalf("empty edge endpoint: %+v", e)
		}
	}
	if exp.Meta.EdgeCount != 3 {
		t.Fatalf("want 3 note-note edges (the tag edge excluded), got %d", exp.Meta.EdgeCount)
	}

	byID := map[string]ExportNode{}
	for _, n := range exp.Nodes {
		byID[n.ID] = n
	}
	// Galaxy orbit: index at 0, its neighbors at 1.
	if byID["hub"].Orbit != 0 {
		t.Fatalf("index note orbit must be 0, got %d", byID["hub"].Orbit)
	}
	if byID["alpha"].Orbit != 1 || byID["beta"].Orbit != 1 {
		t.Fatalf("hub's neighbors should be orbit 1: alpha=%d beta=%d", byID["alpha"].Orbit, byID["beta"].Orbit)
	}
	// Tags surfaced from tagged edges.
	if len(byID["hub"].Tags) != 1 || byID["hub"].Tags[0] != "core" {
		t.Fatalf("hub should carry the 'core' tag, got %v", byID["hub"].Tags)
	}
	// Size grows with degree.
	if byID["hub"].Size <= byID["alpha"].Size {
		t.Fatalf("higher-degree note should have a larger size: hub=%v alpha=%v", byID["hub"].Size, byID["alpha"].Size)
	}
	// Communities all carry a valid color.
	if len(exp.Communities) == 0 {
		t.Fatal("expected at least one community")
	}
	for _, c := range exp.Communities {
		if !hex.MatchString(c.Color) {
			t.Fatalf("community %d has a bad color %q", c.ID, c.Color)
		}
	}
	if exp.Meta.Vault != "/vault" {
		t.Fatalf("vault root should be carried for the editor bridge, got %q", exp.Meta.Vault)
	}
}

func TestBuildExportEmpty(t *testing.T) {
	exp := BuildExport(graph.New(), "/vault")
	if exp.Meta.NodeCount != 0 || len(exp.Nodes) != 0 || exp.Meta.IndexID != "" {
		t.Fatalf("empty graph should export nothing, got %+v", exp.Meta)
	}
}
