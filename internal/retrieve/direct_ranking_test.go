// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

const receiptQuery = "Orbit v1.2.3 activated for local reader only"

func receiptCompetition() []noteSrc {
	return []noteSrc{
		{"receipt.md", "---\nid: receipt\ntype: note\ntitle: " + receiptQuery + "\nrelated: [warning]\n---\n# " + receiptQuery + "\n" + strings.Repeat("Verified executable checksum and preserved configuration. ", 30)},
		{"warning.md", "---\nid: warning\ntype: gotcha\ntitle: Orbit v1.2.3 reader activation warning\nrelated: [older]\n---\n# Orbit v1.2.3 reader activation warning\n" + strings.Repeat(receiptQuery+". ", 3) + "Keep the recovery procedure available.\n"},
		{"older.md", "---\nid: older\ntype: gotcha\ntitle: Recovery procedure\n---\n# Recovery procedure\nKeep the previous executable until verification succeeds.\n"},
	}
}

func TestDirectReceiptSurvivesLinkedWarningCompetition(t *testing.T) {
	r := buildVaultFrom(t, receiptCompetition())
	for _, limit := range []int{1, 2, 3} {
		for _, weights := range [][2]float64{{1, 0}, {0, 1}, {0.7, 0.3}} {
			t.Run(fmt.Sprintf("limit%d-weights%v", limit, weights), func(t *testing.T) {
				fts, err := r.store.Search(context.Background(), receiptQuery, limit)
				if err != nil {
					t.Fatal(err)
				}
				for i, hit := range fts {
					t.Logf("FTS candidate%d id=%s raw=%g", i+1, hit.NodeID, hit.Score)
				}
				for i, hit := range r.ranker.Score(receiptQuery, limit) {
					t.Logf("graph candidate%d id=%s raw=%g", i+1, hit.Node.ID, hit.Score)
				}
				cards, err := r.Retrieve(context.Background(), receiptQuery, Options{Limit: limit, NoRerank: true, WeightFTS: weights[0], WeightGraph: weights[1]})
				if err != nil {
					t.Fatal(err)
				}
				for i, card := range cards {
					t.Logf("final%d id=%s score=%g reason=%s", i+1, card.NoteID, card.Score, card.Reason)
				}
				if len(cards) != limit {
					t.Fatalf("result cap changed: got %v", cardIDs(cards))
				}
				if cards[0].NoteID != "receipt" {
					t.Fatalf("full-title navigation lost its candidate: got %v", cardIDs(cards))
				}
				if limit >= 2 && cards[1].NoteID != "warning" {
					t.Fatalf("relevant direct warning must remain beside receipt: got %v", cardIDs(cards))
				}
				if limit == 3 && cards[2].NoteID != "older" {
					t.Fatalf("linked history must stay reachable: got %v", cardIDs(cards))
				}
			})
		}
	}
}

func TestLinkedWarningStillOutranksWeakDirectMatch(t *testing.T) {
	const query = "alpha beta gamma delta epsilon zeta"
	r := buildVaultFrom(t, []noteSrc{
		{"seed.md", "---\nid: seed\ntitle: " + query + "\nrelated: [warning]\n---\n# " + query + "\n" + query},
		{"weak.md", "---\nid: weak\ntitle: alpha " + strings.Repeat("neutral ", 100) + "\n---\n# Background\nalpha " + strings.Repeat("neutral ", 1000)},
		{"warning.md", "---\nid: warning\ntype: gotcha\ntitle: Recovery procedure\n---\n# Recovery procedure\nKeep the previous executable until verification succeeds.\n"},
	})
	for _, weights := range [][2]float64{{1, 0}, {0, 1}, {0.7, 0.3}} {
		cards, err := r.Retrieve(context.Background(), query, Options{Limit: 3, NoRerank: true, WeightFTS: weights[0], WeightGraph: weights[1]})
		if err != nil {
			t.Fatal(err)
		}
		if len(cards) != 3 || cards[0].NoteID != "seed" || cards[1].NoteID != "warning" || cards[2].NoteID != "weak" || !strings.HasPrefix(cards[1].Reason, "linked from ") {
			t.Fatalf("weights=%v: useful linked warning must beat weak keyword match: %+v", weights, cards)
		}
	}
}

func TestDirectCompetitionKeepsTokenAndResultCaps(t *testing.T) {
	r := buildVaultFrom(t, receiptCompetition())
	for _, budget := range []int{96, 256, 512} {
		var economics Economics
		cards, err := r.Retrieve(context.Background(), receiptQuery, Options{Limit: 2, Budget: budget, Economics: &economics})
		if err != nil {
			t.Fatal(err)
		}
		if len(cards) > 2 || TotalTokens(cards) > budget || economics.ReturnedTokens != TotalTokens(cards) || economics.RerankCalled || economics.SemanticConfigured {
			t.Fatalf("budget=%d: result/token/model contract changed: cards=%+v economics=%+v", budget, cards, economics)
		}
		if budget == 512 && (len(cards) != 2 || cards[0].NoteID != "receipt" || cards[1].NoteID != "warning") {
			t.Fatalf("budget=%d should fit warning and receipt: %+v", budget, cards)
		}
	}
}
