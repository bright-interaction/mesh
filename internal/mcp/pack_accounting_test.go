// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/bright-interaction/mesh/internal/retrieve"
)

// importedBulkVault builds a vault of n connector-ingested notes that all match one
// query. Every note lives under imported/<connector>/, which is what makes labelCard
// rewrite its snippet, so the whole result set is the shape the accounting must survive.
func importedBulkVault(t *testing.T, n int) *Server {
	t.Helper()
	files := map[string]string{}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("slack-msg-%04d", i)
		files["imported/slack/"+id+".md"] = "---\nid: " + id + "\ntype: note\nwhen: 2026-01-01\n" +
			"source: import:slack\nsource_url: https://slack.example/archives/C1/p" + id + "\n---\n" +
			"# deploy pipeline thread " + id + "\n" +
			"the post-receive hook enqueues a deploy build on the server and the runner " +
			"picks it up, so a push is not a deploy until the run reaches a terminal state.\n"
	}
	return serverWithFiles(t, files)
}

// searchWire runs mesh_search and returns the exact bytes of the cards array the agent
// receives, how many cards that is, and the token total the tool reports for them.
func searchWire(t *testing.T, s *Server, query string, budget int) (cardBytes []byte, cards int, reported int) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"query": query, "budget": budget, "limit": 100})
	res, rerr := s.toolSearch(context.Background(), raw)
	if rerr != nil {
		t.Fatalf("toolSearch: %d %s", rerr.Code, rerr.Message)
	}
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("unexpected result type %T", res)
	}
	content, ok := m["content"].([]map[string]any)
	if !ok || len(content) == 0 {
		t.Fatalf("no content in tool result: %v", m)
	}
	var payload struct {
		Cards  json.RawMessage `json:"cards"`
		Tokens int             `json:"tokens"`
	}
	if err := json.Unmarshal([]byte(content[0]["text"].(string)), &payload); err != nil {
		t.Fatalf("tool text not json: %v", err)
	}
	var list []json.RawMessage
	if err := json.Unmarshal(payload.Cards, &list); err != nil {
		t.Fatalf("cards not an array: %v", err)
	}
	return payload.Cards, len(list), payload.Tokens
}

// TestSearchBudgetPricesTheBytesItSends is the regression for mesh_search rewriting
// cards AFTER the packer had priced them. labelCard wraps every connector-ingested
// snippet in the provenance envelope and adds a Source field, so the packer used to
// budget one payload and the tool then sent a bigger one. The budget parameter exists
// so an agent can bound what a call costs its context, which makes an accounting that
// is invalidated after the fact a silent lie to every caller.
//
// Measured on this fixture (300 imported notes), packing with the plain card cost:
//
//	budget  500:  5 cards, reported  450, really   638 (+27.6%)
//	budget 1000: 11 cards, reported  990, really  1400 (+40.0%)
//	budget 2000: 22 cards, reported 1980, really  2797 (+39.9%)
//	budget 3000: 33 cards, reported 2970, really  4194 (+39.8%)
//	budget 8000: 89 cards, reported 7988, really 11284 (+41.0%)  <- the default
//
// Packing with searchCardTokens instead: 4/7/15/23/62 cards, every payload under its
// budget (-2.2%, -10.8%, -4.6%, -2.5%, -1.5%) and reported within 1% of the truth.
func TestSearchBudgetPricesTheBytesItSends(t *testing.T) {
	s := importedBulkVault(t, 300)
	tests := []struct {
		name   string
		budget int
	}{
		{name: "budget 500", budget: 500},
		{name: "budget 1000", budget: 1000},
		{name: "budget 2000", budget: 2000},
		{name: "budget 3000 (the ask/curator default)", budget: 3000},
		{name: "budget 8000 (the search default)", budget: 8000},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cardBytes, cards, reported := searchWire(t, s, "deploy pipeline runner build", tc.budget)
			if cards == 0 {
				t.Fatal("search returned no cards")
			}
			wire := retrieve.EstimateTokens(string(cardBytes))
			// One card is always returned even when it alone busts the budget (the
			// packer's documented floor), so the budget is only binding above that.
			if cards > 1 && wire > tc.budget {
				t.Errorf("sent %d tokens of cards against a %d budget (%d cards): the packer "+
					"priced a payload the tool then rewrote", wire, tc.budget, cards)
			}
			if reported < wire-cards-2 {
				t.Errorf("reported %d tokens for a payload that really costs %d", reported, wire)
			}
		})
	}
}

// TestSearchCardTokensPricesTheLabelledForm pins the cost function itself: it has to
// charge for the envelope and the Source field on an imported card, and charge nothing
// extra on a card from the team's own notes (the envelope is only spent where the text
// is third-party, so pricing it everywhere would starve ordinary results).
func TestSearchCardTokensPricesTheLabelledForm(t *testing.T) {
	body := "the post-receive hook enqueues a deploy build and the runner picks it up"
	tests := []struct {
		name     string
		path     string
		labelled bool
	}{
		{name: "connector-ingested note", path: "imported/slack/slack-msg-1.md", labelled: true},
		{name: "own note", path: "decisions/deploy.md", labelled: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := retrieve.Card{
				NodeID: "note:x", NoteID: "x", Title: "Deploy pipeline",
				Path: tc.path, Type: "note", Scope: "dev", Snippet: body,
				Score: 1.2345678901234567, Reason: "fts",
			}
			plain := retrieve.TotalTokens([]retrieve.Card{c})
			got := searchCardTokens(c)
			if tc.labelled {
				if got <= plain {
					t.Errorf("an imported card prices at %d, no more than the unlabelled %d, "+
						"yet the wire form carries the envelope and Source", got, plain)
				}
				return
			}
			if got != plain {
				t.Errorf("an own-note card prices at %d against the unlabelled %d: nothing is "+
					"added to it on the wire", got, plain)
			}
		})
	}
}
