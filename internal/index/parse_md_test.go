// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/graph"
)

const sample = "---\n" +
	"id: mesh\n" +
	"type: entity\n" +
	"title: Mesh\n" +
	"when: 2026-06-15\n" +
	"tags: [knowledge, go]\n" +
	"related: [codeindex, platform]\n" +
	"---\n" +
	"# Mesh\n" +
	"Mesh links to [[codeindex]] and [[platform|the platform]].\n" +
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

type buildCancelAfterChecksContext struct {
	remaining int
	cause     error
	done      chan struct{}
}

func newBuildCancelAfterChecksContext(checks int, cause error) *buildCancelAfterChecksContext {
	return &buildCancelAfterChecksContext{remaining: checks, cause: cause, done: make(chan struct{})}
}

func (*buildCancelAfterChecksContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *buildCancelAfterChecksContext) Done() <-chan struct{} {
	if c.remaining > 0 {
		c.remaining--
		if c.remaining == 0 {
			close(c.done)
		}
	}
	return c.done
}
func (c *buildCancelAfterChecksContext) Err() error {
	select {
	case <-c.done:
		return c.cause
	default:
		return nil
	}
}
func (*buildCancelAfterChecksContext) Value(any) any { return nil }

func graphFingerprint(g *graph.Graph) []string {
	nodes := g.Nodes()
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	out := make([]string, 0, len(nodes)+g.EdgeCount())
	for _, n := range nodes {
		attrs, _ := json.Marshal(n.Attrs)
		out = append(out, fmt.Sprintf("node|%s|%s|%s|%s|%s|%s|%s|%d|%d|%d|%s",
			n.ID, n.Kind, n.Label, n.NoteID, n.NotePath, n.Anchor, n.SourceLoc,
			n.Community, n.Degree, n.KnowledgeDegree, attrs))
		edges := g.Neighbors(n.ID)
		sort.Slice(edges, func(i, j int) bool {
			if edges[i].Target != edges[j].Target {
				return edges[i].Target < edges[j].Target
			}
			return edges[i].Relation < edges[j].Relation
		})
		for _, e := range edges {
			out = append(out, fmt.Sprintf("edge|%s|%s|%s|%s|%g|%g|%s",
				e.Source, e.Target, e.Relation, e.Confidence, e.ConfidenceScore, e.Weight, e.SourceLoc))
		}
	}
	return out
}

func TestParseExtractsStructure(t *testing.T) {
	pn := parse(t, "mesh.md", sample)

	if pn.FM.ID != "mesh" || pn.FM.Type != "entity" {
		t.Fatalf("frontmatter: id=%q type=%q", pn.FM.ID, pn.FM.Type)
	}
	if len(pn.Headings) != 2 || pn.Headings[1].Anchor != "sync" {
		t.Fatalf("headings: %+v", pn.Headings)
	}

	wantLinks := map[string]bool{"codeindex": true, "platform": true, "missing-note": true}
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
	codeindex := parse(t, "codeindex.md", "---\nid: codeindex\ntype: entity\nwhen: 2026-01-01\n---\n# Codeindex\n")
	platform := parse(t, "platform.md", "---\nid: platform\ntype: entity\nwhen: 2026-01-01\n---\n# Platform\n")

	g, issues := BuildGraph([]*ParsedNote{mesh, codeindex, platform})

	if _, ok := g.Node("note:mesh"); !ok {
		t.Fatal("expected note:mesh node")
	}
	if !hasEdge(g.Neighbors("note:mesh"), "note:codeindex", "references") {
		t.Error("expected mesh -> codeindex reference edge")
	}
	if !hasEdge(g.Neighbors("note:mesh"), "note:platform", "references") {
		t.Error("expected mesh -> platform reference edge (from related + body)")
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

func TestBuildGraphContextMatchesWrapper(t *testing.T) {
	notes := []*ParsedNote{
		parse(t, "mesh.md", sample),
		parse(t, "codeindex.md", "---\nid: codeindex\ntype: entity\nwhen: 2026-01-01\n---\n# Codeindex\n"),
		parse(t, "platform.md", "---\nid: platform\ntype: entity\nwhen: 2026-01-01\n---\n# Platform\n"),
	}
	wrapperGraph, wrapperIssues := BuildGraph(notes)
	contextGraph, contextIssues, err := BuildGraphContext(context.Background(), notes)
	if err != nil {
		t.Fatalf("BuildGraphContext: %v", err)
	}
	if !reflect.DeepEqual(contextIssues, wrapperIssues) {
		t.Fatalf("issues differ:\ncontext: %#v\nwrapper: %#v", contextIssues, wrapperIssues)
	}
	if got, want := graphFingerprint(contextGraph), graphFingerprint(wrapperGraph); !reflect.DeepEqual(got, want) {
		t.Fatalf("graphs differ:\ncontext: %v\nwrapper: %v", got, want)
	}
}

func TestBuildGraphContextCancellationReturnsCauseAndNoPartialGraph(t *testing.T) {
	notes := []*ParsedNote{
		parse(t, "mesh.md", sample),
		parse(t, "codeindex.md", "---\nid: codeindex\ntype: entity\nwhen: 2026-01-01\n---\n# Codeindex\n"),
		parse(t, "platform.md", "---\nid: platform\ntype: entity\nwhen: 2026-01-01\n---\n# Platform\n"),
	}
	cause := errors.New("stop graph build")
	ctx := newBuildCancelAfterChecksContext(25, cause)
	g, issues, err := BuildGraphContext(ctx, notes)
	if !errors.Is(err, cause) {
		t.Fatalf("BuildGraphContext error = %v, want cause %v", err, cause)
	}
	if g != nil || issues != nil {
		t.Fatalf("cancelled build exposed partial output: graph=%v issues=%v", g, issues)
	}
}

func TestIdentityIsFrontmatterIdNotFilename(t *testing.T) {
	// Same frontmatter id, different filename: the node identity follows the id,
	// so a rename does not rot the node or its citations (spec section 3.6).
	renamed := parse(t, "renamed-file.md", "---\nid: codeindex\ntype: entity\nwhen: 2026-01-01\n---\n# Codeindex\n")
	g, _ := BuildGraph([]*ParsedNote{renamed})
	if _, ok := g.Node("note:codeindex"); !ok {
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

// TestLinkKeyTrimsEscapedBracket: a markdown-escaped closing bracket, [[note\]], used to
// leave the backslash on the target, so the link resolved to nothing and lint reported a
// dangling reference to a note whose name ended in "\". Authors escape brackets when a
// renderer would otherwise eat them; they mean the note.
func TestLinkKeyTrimsEscapedBracket(t *testing.T) {
	tests := []struct{ in, want string }{
		{`data-pipelines\`, "data-pipelines"},
		{`monitoring-and-alerting\`, "monitoring-and-alerting"},
		{`plain-note`, "plain-note"},
		{`Spaced Note `, "spaced note"},
		{`note.md`, "note"},
		{`Note.MD`, "note"},
		{`note#section`, "note"},
	}
	for _, tc := range tests {
		if got := linkKey(tc.in); got != tc.want {
			t.Errorf("linkKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBuildGraphResolvesCaseInsensitiveMarkdownExtensions(t *testing.T) {
	target := parse(t, "Folder/Note.MD", "---\nid: target\ntype: note\nwhen: 2026-01-01\n---\n# Target\n")
	source := parse(t, "source.md", "---\nid: source\ntype: note\nwhen: 2026-01-01\n---\n# Source\n[[Note.MD]] and [[Folder/Note.MD]]\n")

	g, issues := BuildGraph([]*ParsedNote{source, target})
	for _, issue := range issues {
		if issue.Kind == "broken-link" {
			t.Errorf("a .MD link to an indexed .MD note was reported broken: %+v", issue)
		}
	}
	if !hasEdge(g.Neighbors("note:source"), "note:target", "references") {
		t.Fatal("case-insensitive .MD links did not resolve to the target note")
	}
}
