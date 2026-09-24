// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestRetrieveDistinguishesRequestedPatchVersion(t *testing.T) {
	var src []noteSrc
	for i, id := range []string{"a-old", "z-requested"} {
		title := fmt.Sprintf("Mesh v0.41.%d activated for local MCP reader only", i+1)
		src = append(src, noteSrc{id + ".md", fmt.Sprintf("---\nid: %s\ntitle: %s\ntype: note\n---\n# %s\n", id, title, title)})
	}
	r := buildVaultFrom(t, src)
	for _, limit := range []int{1, 2} {
		for _, weights := range [][2]float64{{1, 0}, {0, 1}, {0.7, 0.3}} {
			cards, err := r.Retrieve(context.Background(), "Mesh v0.41.2 activated for local MCP reader only", Options{Limit: limit, NoRerank: true, WeightFTS: weights[0], WeightGraph: weights[1]})
			if err != nil {
				t.Fatal(err)
			}
			if len(cards) != limit || cards[0].NoteID != "z-requested" {
				t.Fatalf("limit=%d weights=%v: cards=%+v, want requested patch version first", limit, weights, cards)
			}
		}
	}
}

func TestVersionLookupDoesNotClaimWrongVersionIsExact(t *testing.T) {
	query := "Mesh v0.41.2 activated for local MCP reader only"
	cards := []Card{
		{Title: strings.ReplaceAll(query, "v0.41.2", "v0.41.1"), Reason: "fts", Score: 1},
		{Title: "Other", Reason: "fts", Score: 0.1},
	}
	if call, route := subscriptionRoute(query, cards, "auto"); !call || route != "model" {
		t.Fatalf("wrong patch called exact/confident: call=%t route=%q", call, route)
	}
	cards[0].Title = query
	if call, route := subscriptionRoute(query, cards, "auto"); call || route != "local_exact" {
		t.Fatalf("matching version should stay model-free: call=%t route=%q", call, route)
	}
	cards[0].Title = "Reader activation"
	cards[0].Snippet = "Mesh [v0.41.2] activated for local MCP reader only"
	if call, route := subscriptionRoute(query, cards, "auto"); call || route != "local_confident" {
		t.Fatalf("snippet version should remain usable without inference: call=%t route=%q", call, route)
	}
}

func TestVersionSearchSnippetCannotInventStableConfidence(t *testing.T) {
	query := "Mesh v1.2.3 reader activation"
	for _, version := range []string{"v1.2.3-rc.1", "v1.2.3+build.1", "v1.2.3.4"} {
		t.Run(version, func(t *testing.T) {
			r := buildVaultFrom(t, []noteSrc{{"release.md", "---\nid: release\ntitle: Reader activation\ntype: note\n---\n# Reader activation\n\nMesh " + version + " reader activation\n"}})
			cards, err := r.Retrieve(context.Background(), query, Options{Limit: 1, NoRerank: true, WeightFTS: 1})
			if err != nil || len(cards) != 1 {
				t.Fatalf("search cards=%+v err=%v", cards, err)
			}
			// Keep the real search snippet, but guarantee a large score margin so
			// only literal coverage determines whether routing claims confidence.
			cards[0].Score, cards[0].Reason = 1, "fts"
			cards = append(cards, Card{Title: "Other", Reason: "fts", Score: 0.1})
			if call, route := subscriptionRoute(query, cards, "auto"); !call || route != "model" {
				t.Fatalf("search snippet invented stable confidence: call=%t route=%q snippet=%q", call, route, cards[0].Snippet)
			}
		})
	}
}

func TestVersionSearchSnippetWindowCannotInventStableConfidence(t *testing.T) {
	// Search uses a 12-token excerpt. With the two title words and reader,
	// six filler words place the stable core exactly at its right boundary.
	for _, version := range []string{"1.2.3-rc.1", "1.2.3+build.1", "1.2.3.4"} {
		t.Run(version, func(t *testing.T) {
			body := "# Release notes\n\nreader " + strings.Repeat("filler ", 6) + version + " after\n"
			r := buildVaultFrom(t, []noteSrc{{"release.md", "---\nid: release\ntitle: Release notes\ntype: note\n---\n" + body}})
			cards, err := r.Retrieve(context.Background(), "reader 1.2.3", Options{Limit: 1, NoRerank: true, WeightFTS: 1})
			if err != nil || len(cards) != 1 {
				t.Fatalf("search cards=%+v err=%v", cards, err)
			}
			cards[0].Score, cards[0].Reason = 1, "fts"
			cards = append(cards, Card{Title: "Other", Reason: "fts", Score: 0.1})
			if call, route := subscriptionRoute("reader 1.2.3", cards, "auto"); !call || route != "model" {
				t.Fatalf("truncated snippet invented stable confidence: call=%t route=%q snippet=%q", call, route, cards[0].Snippet)
			}
		})
	}
}

func TestVersionSearchSnippetContinuationCannotInventStableConfidence(t *testing.T) {
	for _, version := range []string{"1.2.3.41", "41.1.2.3"} {
		t.Run(version, func(t *testing.T) {
			r := buildVaultFrom(t, []noteSrc{{"release.md", "---\nid: release\ntitle: Release notes\ntype: note\n---\nreader " + version + " after\n"}})
			query := "reader 1.2.3 41"
			cards, err := r.Retrieve(context.Background(), query, Options{Limit: 1, NoRerank: true, WeightFTS: 1})
			if err != nil || len(cards) != 1 {
				t.Fatalf("search cards=%+v err=%v", cards, err)
			}
			cards[0].Score, cards[0].Reason = 1, "fts"
			cards = append(cards, Card{Title: "Other", Reason: "fts", Score: 0.1})
			if call, route := subscriptionRoute(query, cards, "auto"); !call || route != "model" {
				t.Fatalf("highlighted continuation invented stable confidence: call=%t route=%q snippet=%q", call, route, cards[0].Snippet)
			}
		})
	}
}

func TestVersionRetrievalKeepsScopeAndPathBoundaries(t *testing.T) {
	var src []noteSrc
	for i := 0; i < 8; i++ {
		src = append(src, noteSrc{fmt.Sprintf("private%d.md", i), fmt.Sprintf("---\nid: private%d\nscope: private\ntitle: Mesh v0.41.2 reader activation\n---\n# Mesh v0.41.2 reader activation\n", i)})
	}
	src = append(src,
		noteSrc{"fenced.md", "---\nid: fenced\nscope: dev\ntitle: Mesh v0.41.2 reader activation\n---\n# Mesh v0.41.2 reader activation\n"},
		noteSrc{"allowed.md", "---\nid: allowed\nscope: dev\ntitle: Mesh v0.41.2 reader activation\n---\n# Mesh v0.41.2 reader activation\n"},
		noteSrc{"older.md", "---\nid: older\nscope: dev\ntitle: Mesh v0.41.1 reader activation\n---\n# Mesh v0.41.1 reader activation\n"})
	r := buildVaultFrom(t, src)
	for _, weights := range [][2]float64{{1, 0}, {0, 1}, {0.7, 0.3}} {
		opt := Options{Limit: 1, NoRerank: true, WeightFTS: weights[0], WeightGraph: weights[1], AllowedScopes: map[string]bool{"dev": true}, AllowPath: func(path string) bool { return path == "allowed.md" || path == "older.md" }}
		cards, err := r.Retrieve(context.Background(), "Mesh v0.41.2 reader activation", opt)
		if err != nil {
			t.Fatal(err)
		}
		if len(cards) != 1 || cards[0].NoteID != "allowed" {
			t.Fatalf("scope/path limit starved or leaked: %+v", cards)
		}
		opt.Limit = 10
		for _, query := range []string{"compare Mesh v0.41.1 and v0.41.2 reader activation", "Mesh v0.99.9 reader activation"} {
			cards, err := r.Retrieve(context.Background(), query, opt)
			if err != nil {
				t.Fatal(err)
			}
			if len(cards) != 2 {
				t.Fatalf("comparison/absent version must retain readable context: %q: %+v", query, cards)
			}
			for _, card := range cards {
				if card.NoteID != "allowed" && card.NoteID != "older" {
					t.Fatalf("forbidden card: %+v", card)
				}
			}
		}
	}
}
