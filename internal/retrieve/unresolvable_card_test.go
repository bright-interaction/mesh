// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"testing"

	"github.com/bright-interaction/mesh/internal/graph"
)

// The FTS index and the in-memory graph refresh independently, so a long-running
// `mesh mcp --watch` can match a note whose node its graph has not loaded yet. The card
// builder used to return a shell in that case: NodeID set, Title/Path/NoteID empty.
//
// That is worse than returning nothing. The caller gets a search result it cannot fetch,
// cannot open, and cannot even name, and it still consumes the token budget it was ranked
// into. Seen on the live vault against a daemon that had been up 10 hours: a real,
// correctly-indexed post-mortem came back as an anonymous row.
func TestUnresolvableNodeYieldsNoCard(t *testing.T) {
	g := graph.NewSized(1)
	g.AddNode(&graph.Node{
		ID: "note:present", Kind: "note", Label: "A Present Note",
		NoteID: "present", NotePath: "notes/present.md",
	})
	r := &Retriever{graph: g}

	if _, ok := r.card("note:missing-from-graph"); ok {
		t.Error("a node absent from the graph must be reported as unresolvable so the " +
			"caller can drop it, not returned as a usable card")
	}

	c, ok := r.card("note:present")
	if !ok {
		t.Fatal("a node present in the graph must resolve")
	}
	if c.Title != "A Present Note" || c.Path != "notes/present.md" || c.NoteID != "present" {
		t.Errorf("a resolvable card must carry the fields the caller needs to act on it, got %+v", c)
	}
}

// A card the caller cannot act on is defined by missing identity, so state that directly:
// any card handed back must be fetchable.
func TestEveryReturnedCardIsActionable(t *testing.T) {
	g := graph.NewSized(1)
	g.AddNode(&graph.Node{
		ID: "note:real", Kind: "note", Label: "Real", NoteID: "real", NotePath: "notes/real.md",
	})
	r := &Retriever{graph: g}

	for _, id := range []string{"note:real", "note:ghost"} {
		c, ok := r.card(id)
		if !ok {
			continue // dropped, which is the correct outcome
		}
		if c.NoteID == "" || c.Path == "" || c.Title == "" {
			t.Errorf("card %q was returned as usable but has no identity: %+v", id, c)
		}
	}
}
