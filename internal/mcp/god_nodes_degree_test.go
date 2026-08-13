// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"fmt"
	"strings"
	"testing"
)

// hubAndLongNoteVault is the shape that broke orientation: one note with 30 headings
// and not a single link, and one genuine hub that 12 notes link to and that links back
// to all 12. Raw fan-out ranks the long note first (30 "contains" edges beat the hub's
// 24 "references" edges); knowledge degree ranks the hub first, which is the truth.
func hubAndLongNoteVault(t *testing.T) *Server {
	t.Helper()
	notes := map[string]string{}

	var long strings.Builder
	long.WriteString("---\nid: longnote\ntype: note\nwhen: 2026-01-01\n---\n# Long Note\n")
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&long, "\n## Section %d\nbody text %d, no links at all.\n", i, i)
	}
	notes["longnote.md"] = long.String()

	var hub strings.Builder
	hub.WriteString("---\nid: hub\ntype: note\nwhen: 2026-01-01\n---\n# Hub\n")
	for i := 1; i <= 12; i++ {
		fmt.Fprintf(&hub, "- [[leaf%02d]]\n", i)
		notes[fmt.Sprintf("leaf%02d.md", i)] = fmt.Sprintf(
			"---\nid: leaf%02d\ntype: note\nwhen: 2026-01-01\n---\n# Leaf %d\nback to [[hub]].\n", i, i)
	}
	notes["hub.md"] = hub.String()

	return serverWithNotes(t, notes)
}

// mesh_god_nodes is STEP 1 of the retrieval contract: the first thing an agent reads.
// It must name the vault's real hubs and report a link count that is true, so a long
// unlinked note can never present itself as the entry point with "30 links".
func TestGodNodesRankByKnowledgeDegreeNotNoteLength(t *testing.T) {
	s := hubAndLongNoteVault(t)
	out := toolCall(t, s, "mesh_god_nodes", map[string]any{"limit": 100})

	hubs, _ := out["hubs"].([]any)
	if len(hubs) == 0 {
		t.Fatal("god_nodes returned no hubs")
	}
	first := hubs[0].(map[string]any)
	if first["id"] != "hub" {
		t.Fatalf("the most-connected note must be the linked hub, got %q (the long unlinked note wins on raw fan-out)", first["id"])
	}
	if got := first["degree"].(float64); got != 12 {
		t.Errorf("hub links 12 distinct notes, so degree must be 12, got %v", got)
	}
	for _, h := range hubs {
		m := h.(map[string]any)
		if m["id"] == "longnote" {
			if got := m["degree"].(float64); got != 0 {
				t.Errorf("a note with 30 headings and no links has 0 links, but god_nodes reported %v", got)
			}
		}
	}
}

// The same rule governs the cluster view: a community's exemplar is its best entry
// point, so it must be the most-linked member, not the longest page in the cluster.
func TestCommunityExemplarUsesKnowledgeDegree(t *testing.T) {
	s := hubAndLongNoteVault(t)
	out := toolCall(t, s, "mesh_community", map[string]any{"id": "hub"})
	members, _ := out["members"].([]any)
	if len(members) == 0 {
		t.Fatal("community returned no members")
	}
	first := members[0].(map[string]any)
	if first["id"] != "hub" {
		t.Fatalf("hub's community must lead with the hub, got %q", first["id"])
	}
	if got := first["degree"].(float64); got != 12 {
		t.Errorf("member degree must be the knowledge degree (12), got %v", got)
	}
}
