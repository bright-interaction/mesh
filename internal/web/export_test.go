// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

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
	add("gamma", "Gamma")
	g.AddNode(&graph.Node{ID: "tag:core", Kind: "tag", Label: "core"})
	edge := func(s, t, rel string) { g.AddEdge(graph.Edge{Source: s, Target: t, Relation: rel}) }
	edge("note:hub", "note:alpha", "references")
	edge("note:hub", "note:beta", "references")
	edge("note:alpha", "note:beta", "references")
	edge("note:hub", "note:gamma", "references")
	edge("note:hub", "tag:core", "tagged")
	// Production always recomputes degrees once the graph is assembled (BuildGraph and
	// LoadGraph both do), and connectedness now means knowledge degree, which only that
	// pass fills in. A fixture that skips it is not the graph the export sees.
	g.RecomputeDegrees()
	g.DetectCommunities(0)
	return g
}

func TestBuildExport(t *testing.T) {
	exp := BuildExport(linkedGraph(), "/vault", nil, nil)

	// Notes only (the tag node is excluded), index = the most-connected note. hub links
	// three notes, alpha and beta two each, gamma one: the center is a real hub, not
	// whichever note happens to own the most headings.
	if exp.Meta.NodeCount != 4 || len(exp.Nodes) != 4 {
		t.Fatalf("want 4 note nodes, got %d", len(exp.Nodes))
	}
	if exp.Meta.IndexID != "hub" {
		t.Fatalf("index should be the most-connected note (hub), got %q", exp.Meta.IndexID)
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
	if exp.Meta.EdgeCount != 4 {
		t.Fatalf("want 4 note-note edges (the tag edge excluded), got %d", exp.Meta.EdgeCount)
	}

	byID := map[string]ExportNode{}
	for _, n := range exp.Nodes {
		byID[n.ID] = n
	}
	// Galaxy orbit: index at 0, its neighbors at 1.
	if byID["hub"].Orbit != 0 {
		t.Fatalf("index note orbit must be 0, got %d", byID["hub"].Orbit)
	}
	if byID["alpha"].Orbit != 1 || byID["beta"].Orbit != 1 || byID["gamma"].Orbit != 1 {
		t.Fatalf("hub's neighbors should be orbit 1: alpha=%d beta=%d gamma=%d",
			byID["alpha"].Orbit, byID["beta"].Orbit, byID["gamma"].Orbit)
	}
	// Tags surfaced from tagged edges.
	if len(byID["hub"].Tags) != 1 || byID["hub"].Tags[0] != "core" {
		t.Fatalf("hub should carry the 'core' tag, got %v", byID["hub"].Tags)
	}
	// Size grows with connectedness, and connectedness is links: gamma is a one-link
	// leaf, so it must be the smallest dot no matter how long its page is.
	if byID["hub"].Size <= byID["alpha"].Size {
		t.Fatalf("more-connected note should have a larger size: hub=%v alpha=%v", byID["hub"].Size, byID["alpha"].Size)
	}
	if byID["gamma"].Size >= byID["alpha"].Size {
		t.Fatalf("a one-link leaf should be smaller than a two-link note: gamma=%v alpha=%v", byID["gamma"].Size, byID["alpha"].Size)
	}
	if byID["hub"].Degree != 3 || byID["gamma"].Degree != 1 {
		t.Fatalf("exported degree must be distinct linked notes: hub=%d gamma=%d", byID["hub"].Degree, byID["gamma"].Degree)
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
	exp := BuildExport(graph.New(), "/vault", nil, nil)
	if exp.Meta.NodeCount != 0 || len(exp.Nodes) != 0 || exp.Meta.IndexID != "" {
		t.Fatalf("empty graph should export nothing, got %+v", exp.Meta)
	}
}
