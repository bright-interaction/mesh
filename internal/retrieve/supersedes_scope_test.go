// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"context"
	"encoding/json"
	"testing"
)

// P0-1: card() copied Attrs["superseded_by"] onto the returned Card with no scope
// check at all, and boostMult demoted the score whenever that field was non-empty.
// Retrieve filters WHICH cards a scoped caller gets back, but nothing filtered what a
// returned card was allowed to SAY about another node - so a public-scoped caller who
// could never fetch the fenced superseding note (mesh_fetch returns the opaque
// "unknown note id" for it) still received its full id in SupersededBy, and the score
// was halved on top, which is itself a second, cheaper oracle for "something exists
// there" even with the field blanked.
//
// The two tests below build a vault where a fenced (secret-scope) note supersedes a
// publicly-readable one, and an otherwise BYTE-IDENTICAL control vault with no
// supersedes relation at all. WeightGraph/WeightVec are pinned to 0 (WeightFTS: 1) so
// the fused score comes only from the SQLite FTS5 signal over each note's own body
// text. That isolates the score to exactly the thing under test: whether the demotion
// and the field are gated by the same read-boundary predicate Retrieve applies to the
// superseding note. A separate graph-only regression proves the relationship id never
// enters BM25 as searchable target prose.
const supersedesQuery = "widget frobnicator calibration procedure"

func publicNoteSrc() noteSrc {
	return noteSrc{"public-note.md", "---\nid: public-note\ntype: note\nwhen: 2026-01-01\n" +
		"scope: [public]\n---\n# Public Note\n" + supersedesQuery + "\n"}
}

func secretNoteSrc(withSupersedes bool) noteSrc {
	fm := "---\nid: secret-note\ntype: note\nwhen: 2026-01-01\nscope: [secret]\n"
	if withSupersedes {
		fm += "supersedes:\n  - public-note\n"
	}
	fm += "---\n# Secret Note\nunrelated internal content\n"
	return noteSrc{"secret-note.md", fm}
}

// scopedFTSOnlyOptions isolates the fused score to the FTS arm; see the file comment.
func scopedFTSOnlyOptions(allowed map[string]bool) Options {
	return Options{
		Limit:         10,
		AllowedScopes: allowed,
		WeightFTS:     1,
		WeightGraph:   0,
		WeightVec:     0,
	}
}

func publicNoteCard(t *testing.T, cards []Card) Card {
	t.Helper()
	for _, c := range cards {
		if c.NodeID == "note:public-note" {
			return c
		}
	}
	t.Fatal("public-note was not returned")
	return Card{}
}

// TestFencedSupersedesIsInvisibleToACallerWhoCannotReadTheSupersedingNote is the
// repro. It FAILS before the fix (SupersededBy leaks the fenced id and/or the score
// is halved) and PASSES after (the fenced caller's card is byte-identical to one from
// a vault where the supersedes relation never existed).
func TestFencedSupersedesIsInvisibleToACallerWhoCannotReadTheSupersedingNote(t *testing.T) {
	fenced := buildVaultFrom(t, []noteSrc{publicNoteSrc(), secretNoteSrc(true)})
	control := buildVaultFrom(t, []noteSrc{publicNoteSrc(), secretNoteSrc(false)})

	opt := scopedFTSOnlyOptions(map[string]bool{"public": true})

	fencedCards, err := fenced.Retrieve(context.Background(), supersedesQuery, opt)
	if err != nil {
		t.Fatal(err)
	}
	controlCards, err := control.Retrieve(context.Background(), supersedesQuery, opt)
	if err != nil {
		t.Fatal(err)
	}

	fencedCard := publicNoteCard(t, fencedCards)
	controlCard := publicNoteCard(t, controlCards)

	if fencedCard.SupersededBy != "" {
		t.Errorf("SupersededBy leaked a fenced id to a caller who cannot read it: %q", fencedCard.SupersededBy)
	}

	fencedJSON, err := json.Marshal(fencedCard)
	if err != nil {
		t.Fatal(err)
	}
	controlJSON, err := json.Marshal(controlCard)
	if err != nil {
		t.Fatal(err)
	}
	if string(fencedJSON) != string(controlJSON) {
		t.Fatalf("a caller who cannot read the superseding note must get a card byte-identical "+
			"to one from a vault with no supersedes relation at all (score included):\n"+
			"fenced:  %s\ncontrol: %s", fencedJSON, controlJSON)
	}
}

// TestSupersedesStillWorksWhenTheSupersedingNoteIsReadable proves the fix does not
// silently disable the feature: a caller who CAN read the superseding note still gets
// the field populated and the demotion applied, unchanged from before.
func TestSupersedesStillWorksWhenTheSupersedingNoteIsReadable(t *testing.T) {
	// Both notes in the same scope, so a "public"-scoped caller can read both.
	srcs := []noteSrc{
		{"public-note.md", "---\nid: public-note\ntype: note\nwhen: 2026-01-01\n" +
			"scope: [public]\n---\n# Public Note\n" + supersedesQuery + "\n"},
		{"newer-note.md", "---\nid: newer-note\ntype: note\nwhen: 2026-01-01\nscope: [public]\n" +
			"supersedes:\n  - public-note\n---\n# Newer Note\n" + supersedesQuery + "\n"},
	}
	withSupersedes := buildVaultFrom(t, srcs)

	controlSrcs := []noteSrc{
		{"public-note.md", srcs[0].body},
		{"newer-note.md", "---\nid: newer-note\ntype: note\nwhen: 2026-01-01\nscope: [public]\n" +
			"---\n# Newer Note\n" + supersedesQuery + "\n"},
	}
	without := buildVaultFrom(t, controlSrcs)

	opt := scopedFTSOnlyOptions(map[string]bool{"public": true})

	withCards, err := withSupersedes.Retrieve(context.Background(), supersedesQuery, opt)
	if err != nil {
		t.Fatal(err)
	}
	withoutCards, err := without.Retrieve(context.Background(), supersedesQuery, opt)
	if err != nil {
		t.Fatal(err)
	}

	got := publicNoteCard(t, withCards)
	base := publicNoteCard(t, withoutCards)

	if got.SupersededBy != "newer-note" {
		t.Errorf("SupersededBy = %q, want newer-note: the field must still populate when the "+
			"superseding note is readable", got.SupersededBy)
	}
	if !(got.Score < base.Score) {
		t.Errorf("demotion did not apply for a readable superseding note: got score %.6f, "+
			"undemoted control score %.6f", got.Score, base.Score)
	}
}
