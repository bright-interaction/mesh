// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"context"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/embed"
	"github.com/bright-interaction/mesh/internal/index"
)

func TestTitleLookupProtectionIsLiteralAndPreservesScores(t *testing.T) {
	for _, query := range []string{receiptQuery, " \n" + strings.ToUpper(receiptQuery) + "\t"} {
		cards := []Card{
			{NodeID: "warning", Title: "Warning", Score: 1.1},
			{NodeID: "receipt", Title: receiptQuery, Score: 0.02},
			{NodeID: "duplicate", Title: receiptQuery, Score: 0.01},
			{NodeID: "z-linked", Score: 0.44},
			{NodeID: "a-linked", Score: 0.22},
		}
		sortCards(cards)
		prioritizeTitleLookup(query, cards, []string{"warning", "receipt", "duplicate"})
		wantIDs := []string{"receipt", "duplicate", "warning", "z-linked", "a-linked"}
		wantScores := []float64{0.02, 0.01, 1.1, 0.44, 0.22}
		for i, c := range cards {
			if c.NodeID != wantIDs[i] || c.Score != wantScores[i] {
				t.Fatalf("navigation order or unchanged relevance scores lost: %+v", cards)
			}
		}
	}
	for _, query := range []string{
		"", " \t", "Orbit", receiptQuery + " not", "not " + receiptQuery,
		strings.ReplaceAll(receiptQuery, "v1.2.3", "v1.2.30"),
		strings.ReplaceAll(receiptQuery, "v1.2.3", "v1.2.3-rc.1"),
		strings.ReplaceAll(receiptQuery, "v1.2.3", "v1.2.3+build.1"),
		strings.ReplaceAll(receiptQuery, "v1.2.3", "v1-2-3"),
		strings.ReplaceAll(receiptQuery, " only", ""),
		receiptQuery + strings.Repeat(" suffix", 100),
	} {
		cards := []Card{{NodeID: "receipt", Title: receiptQuery, Score: 0.02}, {NodeID: "linked", Score: 0.44}}
		before := append([]Card(nil), cards...)
		prioritizeTitleLookup(query, cards, []string{"receipt"})
		if !reflect.DeepEqual(cards, before) {
			t.Fatalf("nonliteral query %q changed context: %+v", query, cards)
		}
	}
}

func TestTitleLookupRequiresCurrentDirectNonSupersededTarget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		direct []string
		target Card
	}{
		{"expansion-only title", nil, Card{NodeID: "target", Title: receiptQuery, Score: 0.02}},
		{"retired title", []string{"target"}, Card{NodeID: "target", Title: receiptQuery, Score: 0.01, SupersededBy: "correction"}},
		{"current title differs", []string{"target"}, Card{NodeID: "target", Title: "Renamed note", Score: 0.02}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cards := []Card{tc.target, {NodeID: "correction", Score: 0.44}}
			before := append([]Card(nil), cards...)
			prioritizeTitleLookup(receiptQuery, cards, tc.direct)
			if !reflect.DeepEqual(cards, before) {
				t.Fatalf("unexpected protection: %+v", cards)
			}
		})
	}
}

func TestTitleLookupUsesCurrentTitleNotStaleGraph(t *testing.T) {
	for _, currentExact := range []bool{true, false} {
		t.Run(map[bool]string{true: "new exact title", false: "renamed away"}[currentExact], func(t *testing.T) {
			old := receiptCompetition()
			current := receiptCompetition()
			other := "title: Renamed deployment record"
			if currentExact {
				old[0].body = strings.Replace(old[0].body, "title: "+receiptQuery, other, 1)
			} else {
				current[0].body = strings.Replace(current[0].body, "title: "+receiptQuery, other, 1)
			}
			r := buildVaultFrom(t, old)
			var notes []*index.ParsedNote
			for _, src := range current {
				n, err := index.Parse(src.path, []byte(src.body))
				if err != nil {
					t.Fatal(err)
				}
				notes = append(notes, n)
			}
			g, _ := index.BuildGraph(notes)
			if _, err := r.store.IndexVault(notes, g); err != nil {
				t.Fatal(err)
			}
			cards, err := r.Retrieve(context.Background(), receiptQuery, Options{Limit: 2, NoRerank: true, WeightFTS: 1})
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"note:warning", "note:older"}
			if currentExact {
				want = []string{"note:receipt", "note:warning"}
			}
			if !reflect.DeepEqual(cardIDs(cards), want) {
				t.Fatalf("currentExact=%t: got %+v, want %v", currentExact, cards, want)
			}
		})
	}
}

func TestSupersededTitleCannotDemoteLinkedCorrection(t *testing.T) {
	src := receiptCompetition()
	src[2].body = strings.Replace(src[2].body, "type: gotcha", "type: gotcha\nsupersedes: [receipt]", 1)
	r := buildVaultFrom(t, src)
	cards, err := r.Retrieve(context.Background(), receiptQuery, Options{Limit: 3, NoRerank: true, WeightFTS: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 3 || cards[1].NoteID != "older" || cards[2].NoteID != "receipt" || cards[2].SupersededBy != "older" {
		t.Fatalf("linked correction must retain priority over retired exact title: %+v", cards)
	}
	// At cap 1 an explicit historical lookup keeps the requested note as a
	// candidate, not both it and its correction. The replacement pointer must
	// survive; candidate retention is not a guarantee that corrections win cap 1.
	cards, err = r.Retrieve(context.Background(), receiptQuery, Options{Limit: 1, NoRerank: true, WeightFTS: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 1 || cards[0].NoteID != "receipt" || cards[0].SupersededBy != "older" {
		t.Fatalf("historical exact-title lookup lost replacement guidance: %+v", cards)
	}
}

func TestTitleProtectionLeavesRerankerInControl(t *testing.T) {
	r := buildVaultFrom(t, receiptCompetition())
	r.EnableRerank(fakeReranker{needle: "keep the previous executable"})
	cards, err := r.Retrieve(context.Background(), receiptQuery, Options{Limit: 2, WeightFTS: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 2 || cards[0].NoteID != "older" || !strings.Contains(cards[0].Reason, "reranked") {
		t.Fatalf("configured reranker lost authority: %+v", cards)
	}
}

func TestUnreadableExactTitleDoesNotDemoteLinkedContext(t *testing.T) {
	for _, fence := range []string{"scope", "path"} {
		t.Run(fence, func(t *testing.T) {
			src := receiptCompetition()
			opt := Options{Limit: 2, NoRerank: true, WeightFTS: 1}
			if fence == "scope" {
				src[0].body = strings.Replace(src[0].body, "type: note", "type: note\nscope: private", 1)
				opt.AllowedScopes = map[string]bool{"dev": true}
			} else {
				opt.AllowPath = func(path string) bool { return path != "receipt.md" }
			}
			r := buildVaultFrom(t, src)
			cards, err := r.Retrieve(context.Background(), receiptQuery, opt)
			if err != nil {
				t.Fatal(err)
			}
			if len(cards) != 2 || cards[0].NoteID != "warning" || cards[1].NoteID != "older" || math.Abs(cards[1].Score-0.44) > 1e-12 {
				t.Fatalf("unreadable exact title influenced returned context: %+v", cards)
			}
		})
	}
}

func TestTitleNavigationSkipsAutoRerankWithoutRewritingScores(t *testing.T) {
	r := buildVaultFrom(t, receiptCompetition())
	r.EnableRerank(failingMeasuredReranker{policy: "auto"})
	var economics Economics
	cards, err := r.Retrieve(context.Background(), receiptQuery, Options{Limit: 2, WeightFTS: 1, Budget: 1200, Economics: &economics})
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 2 || cards[0].NoteID != "receipt" || cards[1].NoteID != "warning" || cards[0].Score >= cards[1].Score {
		t.Fatalf("navigation must lead without fabricating stronger relevance: %+v", cards)
	}
	if economics.RerankCalled || economics.Route != "local_exact" || economics.ReturnedTokens > 1200 {
		t.Fatalf("explicit lookup should remain local and within budget: %+v", economics)
	}
}

func TestTitleNavigationDoesNotChangeVectorOnlyOrder(t *testing.T) {
	r := buildVaultFrom(t, receiptCompetition())
	stub := embed.Stub{D: 64}
	v, err := stub.Embed(context.Background(), []string{receiptQuery})
	if err != nil {
		t.Fatal(err)
	}
	if !r.EnableVectors(stub, stub.Model(), 64, map[string][][]float32{"note:receipt": v, "note:warning": v}) {
		t.Fatal("enable local vector fixture")
	}
	cards, err := r.Retrieve(context.Background(), receiptQuery, Options{Limit: 2, NoRerank: true, WeightVec: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 2 || cards[0].NoteID != "warning" || cards[1].NoteID != "receipt" || cards[0].Reason != "vector" {
		t.Fatalf("disabled lexical lanes must not affect vector-only ranking: %+v", cards)
	}
}

func TestTitleNavigationUnicodeRouteRemainsLocal(t *testing.T) {
	cards := []Card{{Title: "Σ release receipt", Score: 0.02}, {Title: "warning", Score: 1}}
	if call, route := subscriptionRoute("ς RELEASE RECEIPT", cards, "auto"); call || route != "local_exact" {
		t.Fatalf("Unicode full-title navigation called a model: call=%t route=%s", call, route)
	}
	if call, route := subscriptionRoute("ς RELEASE RECEIPT", cards, "always"); !call || route != "model" {
		t.Fatalf("explicit always policy lost authority: call=%t route=%s", call, route)
	}
}
