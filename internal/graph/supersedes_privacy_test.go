// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package graph

import "testing"

// A relation id is not public note prose. If nodeText indexes superseded_by, a caller
// scoped to public can search a fenced correction's id and make the public target score,
// revealing that the secret note exists even when later card filtering hides the field.
func TestSupersededByDoesNotEnterGraphBM25Text(t *testing.T) {
	g := NewSized(2)
	g.AddNode(&Node{
		ID: "note:public", Kind: "note", Label: "Public target", NoteID: "public", NotePath: "public.md",
		Attrs: map[string]any{"scope": "public", "superseded_by": "classified-replacement-needle"},
	})
	g.AddNode(&Node{
		ID: "note:secret", Kind: "note", Label: "Restricted replacement", NoteID: "secret", NotePath: "secret.md",
		Attrs: map[string]any{"scope": "secret"},
	})

	hits := g.NewRanker().ScoreScoped("classified-replacement-needle", 0, map[string]bool{"public": true})
	if len(hits) != 0 {
		t.Fatalf("fenced superseder id produced a public graph hit: %+v", hits)
	}
}
