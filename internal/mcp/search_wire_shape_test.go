// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/retrieve"
)

// hasKeyNodeID reports whether any object in the decoded JSON tree carries the exact key
// "NodeID". Checking with strings.Contains would false-fail the day a legitimate
// ParentNodeID or NodeIDs field appears, so match the KEY, never the substring.
func hasKeyNodeID(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		if _, ok := t["NodeID"]; ok {
			return true
		}
		for _, sub := range t {
			if hasKeyNodeID(sub) {
				return true
			}
		}
	case []any:
		for _, sub := range t {
			if hasKeyNodeID(sub) {
				return true
			}
		}
	}
	return false
}

// TestNodeIDIsNotOnTheSearchWireEndToEnd asserts on the bytes mesh_search ACTUALLY emits,
// driven through dispatch exactly as an agent reaches it - not on labelCard's return
// value. Testing the helper instead of the handler is a real gap: a fast path added to
// labelCards can bypass labelCard entirely and leave a helper-level test green while the
// wire regresses.
func TestNodeIDIsNotOnTheSearchWireEndToEnd(t *testing.T) {
	s := newTestServer(t)
	res, rerr := s.dispatch(context.Background(), request{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call",
		Params: mustJSON(map[string]any{"name": "mesh_search", "arguments": map[string]any{"query": "sqlite storage"}}),
	})
	if rerr != nil {
		t.Fatalf("rpc error: %v", rerr)
	}
	out := toolText(t, res.(map[string]any))
	cards, _ := out["cards"].([]any)
	if len(cards) == 0 {
		t.Fatal("expected cards")
	}
	if hasKeyNodeID(out) {
		b, _ := json.Marshal(out)
		t.Errorf("NodeID reached the agent (it is exactly notePrefix+NoteID): %s", b)
	}
	first, _ := cards[0].(map[string]any)
	if first["NoteID"] == nil || first["NoteID"] == "" {
		t.Error("NoteID must be present: with NodeID gone it is the ONLY id the agent can pass back")
	}
	// Type is NOT derivable from Path (76 of 2120 notes disagree with their directory:
	// root-level notes have none, entities/<name>-log*.md are type note), so it stays.
	if _, ok := first["Type"]; !ok {
		t.Error("Type must stay on the wire, it is not derivable from Path")
	}
}

// TestSearchReportedTokensCoverWhatIsSent is the budget-honesty gate on an OWN-note vault.
// The pre-existing end-to-end budget test uses an all-imported vault, so the common path -
// a team's own notes - had no end-to-end coverage at all, which is how a labelCards fast
// path shipped 2254 tokens against a 2000 budget while reporting 1975.
func TestSearchReportedTokensCoverWhatIsSent(t *testing.T) {
	s := newTestServer(t)
	const budget = 2000
	res, rerr := s.dispatch(context.Background(), request{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call",
		Params: mustJSON(map[string]any{"name": "mesh_search", "arguments": map[string]any{
			"query": "sqlite storage", "budget": budget}}),
	})
	if rerr != nil {
		t.Fatalf("rpc error: %v", rerr)
	}
	out := toolText(t, res.(map[string]any))
	reported, _ := out["tokens"].(float64)
	cards, _ := out["cards"].([]any)
	b, err := json.Marshal(cards)
	if err != nil {
		t.Fatal(err)
	}
	actual := retrieve.EstimateTokens(string(b))
	if int(reported) > budget {
		t.Errorf("reported %d tokens against a %d budget", int(reported), budget)
	}
	if actual > budget {
		t.Errorf("SENT %d tokens against a %d budget (reported %d): the packer priced a "+
			"different shape than the one emitted", actual, budget, int(reported))
	}
}

// TestSearchCardTokensPricesTheTrimmedShape guards budget accounting AND fails closed if
// the shadow field is ever deleted. searchCardTokens prices what labelCard emits, so
// dropping a field must make the price strictly smaller than the raw card - the id here is
// deliberately long so the difference cannot round away.
func TestSearchCardTokensPricesTheTrimmedShape(t *testing.T) {
	id := "a-very-long-note-identifier-that-costs-real-bytes-on-every-single-card-returned"
	c := retrieve.Card{NodeID: notePrefix + id, NoteID: id, Title: "T", Path: "decisions/x.md", Type: "decision"}

	b, err := json.Marshal(labelCard(c))
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatal(err)
	}
	if hasKeyNodeID(decoded) {
		t.Errorf("priced shape still carries NodeID: %s", b)
	}
	if got, want := searchCardTokens(c), retrieve.EstimateTokens(string(b)); got != want {
		t.Errorf("searchCardTokens = %d, want %d (must price the shape it emits)", got, want)
	}
	// Strictly smaller, not merely no-larger: a <= assertion passes with the shadow field
	// deleted, which is exactly the regression this test exists to catch.
	if got, plain := searchCardTokens(c), retrieve.TotalTokens([]retrieve.Card{c}); got >= plain {
		t.Errorf("trimmed card prices at %d, not below the untrimmed %d: NodeID is back on the wire", got, plain)
	}
	if !strings.Contains(string(b), `"NoteID":"`+id+`"`) {
		t.Errorf("NoteID missing from the priced shape: %s", b)
	}
}
