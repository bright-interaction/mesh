package index

import (
	"testing"

	"github.com/brightinteraction/mesh/internal/graph"
)

const sample = "---\n" +
	"id: mesh\n" +
	"type: entity\n" +
	"title: Mesh\n" +
	"when: 2026-06-15\n" +
	"tags: [knowledge, go]\n" +
	"related: [graphify, dockyard]\n" +
	"---\n" +
	"# Mesh\n" +
	"Mesh links to [[graphify]] and [[dockyard|the platform]].\n" +
	"It is tagged #sovereign here.\n" +
	"## Sync\n" +
	"```\n" +
	"this [[fake-link]] and #fake-tag must be ignored\n" +
	"```\n" +
	"Inline `[[also-ignored]]` stays out too.\n" +
	"See [[missing-note]] which does not exist.\n"

func parse(t *testing.T, path, body string) *ParsedNote {
	t.Helper()
	pn, err := Parse(path, []byte(body))
	if err != nil {
		t.Fatalf("Parse(%s): %v", path, err)
	}
	return pn
}

func TestParseExtractsStructure(t *testing.T) {
	pn := parse(t, "mesh.md", sample)

	if pn.FM.ID != "mesh" || pn.FM.Type != "entity" {
		t.Fatalf("frontmatter: id=%q type=%q", pn.FM.ID, pn.FM.Type)
	}
	if len(pn.Headings) != 2 || pn.Headings[1].Anchor != "sync" {
		t.Fatalf("headings: %+v", pn.Headings)
	}

	wantLinks := map[string]bool{"graphify": true, "dockyard": true, "missing-note": true}
	for _, l := range pn.Links {
		if !wantLinks[l.Target] {
			t.Errorf("unexpected link target %q (code/inline should be skipped)", l.Target)
		}
		delete(wantLinks, l.Target)
	}
	if len(wantLinks) != 0 {
		t.Errorf("missing links: %v", wantLinks)
	}

	for _, tag := range pn.Tags {
		if tag.Name == "fake-tag" {
			t.Error("tag inside code fence must be ignored")
		}
	}
	var hasSovereign bool
	for _, tag := range pn.Tags {
		if tag.Name == "sovereign" {
			hasSovereign = true
		}
	}
	if !hasSovereign {
		t.Error("expected #sovereign tag")
	}
}

func TestBuildGraphResolvesAndFlags(t *testing.T) {
	mesh := parse(t, "mesh.md", sample)
	graphify := parse(t, "graphify.md", "---\nid: graphify\ntype: entity\nwhen: 2026-01-01\n---\n# Graphify\n")
	dockyard := parse(t, "dockyard.md", "---\nid: dockyard\ntype: entity\nwhen: 2026-01-01\n---\n# Dockyard\n")

	g, issues := BuildGraph([]*ParsedNote{mesh, graphify, dockyard})

	if _, ok := g.Node("note:mesh"); !ok {
		t.Fatal("expected note:mesh node")
	}
	if !hasEdge(g.Neighbors("note:mesh"), "note:graphify", "references") {
		t.Error("expected mesh -> graphify reference edge")
	}
	if !hasEdge(g.Neighbors("note:mesh"), "note:dockyard", "references") {
		t.Error("expected mesh -> dockyard reference edge (from related + body)")
	}
	if !hasEdge(g.Neighbors("note:mesh"), "tag:sovereign", "tagged") {
		t.Error("expected mesh -> sovereign tagged edge")
	}

	var broken int
	for _, is := range issues {
		if is.Kind == "broken-link" {
			broken++
		}
	}
	if broken != 1 {
		t.Errorf("expected 1 broken link (missing-note), got %d: %v", broken, issues)
	}
}

func TestIdentityIsFrontmatterIdNotFilename(t *testing.T) {
	// Same frontmatter id, different filename: the node identity follows the id,
	// so a rename does not rot the node or its citations (spec section 3.6).
	renamed := parse(t, "renamed-file.md", "---\nid: graphify\ntype: entity\nwhen: 2026-01-01\n---\n# Graphify\n")
	g, _ := BuildGraph([]*ParsedNote{renamed})
	if _, ok := g.Node("note:graphify"); !ok {
		t.Fatal("node id must be note:<frontmatter-id>, not note:<filename>")
	}
	if _, ok := g.Node("note:renamed-file"); ok {
		t.Fatal("node id must not be derived from the filename when an id is present")
	}
}

func TestMissingIdFallsBackToFilenameWithIssue(t *testing.T) {
	noID := parse(t, "orphan.md", "# Orphan\nno frontmatter here\n")
	g, issues := BuildGraph([]*ParsedNote{noID})
	if _, ok := g.Node("note:orphan"); !ok {
		t.Fatal("expected fallback node id note:orphan")
	}
	var found bool
	for _, is := range issues {
		if is.Kind == "missing-id" {
			found = true
		}
	}
	if !found {
		t.Error("expected a missing-id issue for the file without frontmatter")
	}
}

func hasEdge(edges []graph.Edge, target, rel string) bool {
	for _, e := range edges {
		if e.Target == target && e.Relation == rel {
			return true
		}
	}
	return false
}
