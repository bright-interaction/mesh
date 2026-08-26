// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"context"
	"strings"
	"testing"
)

// TestSupersededNoteIsDemotedButStillReachable is the ranking half of the note-level
// UPDATE path. Marking the graph node is inert unless retrieval acts on it, and the
// failure it exists to stop is specific: a corrected diagnosis is written as a NEW note
// while the superseded one keeps its rank and keeps being retrieved FIRST, so the next
// session reads the wrong answer and re-derives from there.
//
// Both notes match the query identically and share a type, so tier-0 and freshness are
// equal and the only thing separating them is supersededMult.
func TestSupersededNoteIsDemotedButStillReachable(t *testing.T) {
	const query = "sqlitebusy wal checkpoint contention diagnosis"
	srcs := []noteSrc{
		{"old-diagnosis.md", "---\nid: old-diagnosis\ntype: gotcha\nwhen: 2026-01-01\n" +
			"---\n# Old\n" + query + "\n"},
		{"new-diagnosis.md", "---\nid: new-diagnosis\ntype: gotcha\nwhen: 2026-01-01\n" +
			"supersedes:\n  - old-diagnosis\n---\n# New\n" + query + "\n"},
	}
	r := buildVaultFrom(t, srcs)

	cards, err := r.Retrieve(context.Background(), query, Options{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}

	var oldCard, newCard *Card
	var oldRank, newRank int
	for i := range cards {
		switch cards[i].NodeID {
		case "note:old-diagnosis":
			oldCard, oldRank = &cards[i], i
		case "note:new-diagnosis":
			newCard, newRank = &cards[i], i
		}
	}

	// DEMOTED, not hidden. The superseded note is often the only record of what was tried
	// and ruled out; burying it invites the next session to re-derive the same wrong
	// answer, which is the exact failure this mechanism exists to prevent.
	if oldCard == nil {
		t.Fatal("the superseded note vanished from results: demotion must not be deletion")
	}
	if newCard == nil {
		t.Fatal("the superseding note is missing from results")
	}
	if newRank > oldRank {
		t.Fatalf("the superseded note outranked its own correction (old at %d, new at %d)", oldRank, newRank)
	}
	if oldCard.Score >= newCard.Score {
		t.Fatalf("superseded score %.4f >= superseding score %.4f: the demotion did not apply",
			oldCard.Score, newCard.Score)
	}

	// The card must say WHICH note replaced it. A demoted note that still surfaces is
	// useful; a demoted note that surfaces unlabelled is a trap.
	if oldCard.SupersededBy != "new-diagnosis" {
		t.Errorf("SupersededBy = %q, want new-diagnosis", oldCard.SupersededBy)
	}
	if newCard.SupersededBy != "" {
		t.Errorf("the superseding note was itself marked superseded: %q", newCard.SupersededBy)
	}
}

// The demotion must REACH the reranked head. boostMult exists because rerankHead
// ASSIGNS head[i].Score, so any multiplier applied only in the card loop is discarded
// for the whole head - and rerankK (30) exceeds the default limit, which makes the head
// the entire returned set. A supersedes demotion that silently vanished under a
// configured reranker would be worse than none, because it would look like it worked.
//
// The stronger claim, that the demotion beats a confident reranker, was tried here
// first and it FAILED. rerankHead computes rel = a*norm[i] + (1-a)*fusedNorm[i] with
// the shipped rerankBlendDefault = 1.0, so the fused signal gets zero weight, and norm
// comes from minMaxFloored, which stretches the head's worst and best raw scores to
// [normFloor, 1.0] regardless of how close together they actually were - it discards
// magnitude. Measured with a fake reranker that prefers the superseded note by one
// percentage point (0.81 vs 0.80, about as unconfident a preference as exists), the
// superseded note still wins: old=1.550000, new=1.022000. Bisection puts the flip
// threshold at exactly normFloor (0.02), roughly 25x smaller than the shipped
// supersededMult (0.5). So it is not only a maximally-confident reranker that outranks
// the demotion; a minimally-confident one does too. This is not universal: padding the
// rerank head with other candidates that score below the pair restores correct
// ordering (measured new=2.0780 vs old=1.5500), because that pulls the pair off the
// head's own min/max and lets the multiplier separate them again. The failure is
// specific to the superseded note and its replacement spanning the head's own score
// range, which is the likely shape for a small vault or a narrow query. Rerank is
// opt-in and off by default (Go constructor gate, production docker-compose.yml, and
// the live launchd plist all agree), so this is latent rather than a live production
// bug - but it means the current behavior, that an editorial "this is retired" can be
// outranked instead of vetoing, follows from these two constants by accident, not from
// a decision anyone made. Whether a superseded note should ever be able to veto a
// strong content match, or should only ever cost rank no matter how confident the
// reranker, is left open here on purpose.
//
// What this test actually proves is the weaker, honest claim: the multiplier REACHES
// the reranked head at all, nothing about where it lands relative to the reranker's own
// preference. It measures the same note, with and without the supersedes relation,
// under an identical reranker - the only variable is the demotion.
func TestSupersededDemotionReachesTheRerankedHead(t *testing.T) {
	const query = "sqlitebusy wal checkpoint contention diagnosis"
	oldNote := "---\nid: old-diagnosis\ntype: gotcha\nwhen: 2026-01-01\n---\n# Old\n" + query + " oldmarker\n"

	scoreOfOld := func(t *testing.T, newNote string) float64 {
		t.Helper()
		r := buildVaultFrom(t, []noteSrc{
			{"old-diagnosis.md", oldNote},
			{"new-diagnosis.md", newNote},
		})
		// The reranker PREFERS the old note, putting it at the top of the head with a
		// normalized relevance of 1, so the head path is definitely the one exercised.
		r.EnableRerank(fakeReranker{needle: "oldmarker"})
		cards, err := r.Retrieve(context.Background(), query, Options{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cards {
			if c.NodeID == "note:old-diagnosis" {
				if !strings.Contains(c.Reason, "reranked") {
					t.Fatalf("the reranked head was not exercised; reason=%q", c.Reason)
				}
				return c.Score
			}
		}
		t.Fatal("the old note was not returned")
		return 0
	}

	withoutSupersedes := scoreOfOld(t, "---\nid: new-diagnosis\ntype: gotcha\nwhen: 2026-01-01\n---\n# New\n"+query+"\n")
	withSupersedes := scoreOfOld(t, "---\nid: new-diagnosis\ntype: gotcha\nwhen: 2026-01-01\nsupersedes:\n  - old-diagnosis\n---\n# New\n"+query+"\n")

	if withSupersedes >= withoutSupersedes {
		t.Fatalf("the demotion did not reach the reranked head: with=%.4f, without=%.4f "+
			"(rerankHead assigns Score, so a multiplier applied only in the card loop is lost here)",
			withSupersedes, withoutSupersedes)
	}
}
