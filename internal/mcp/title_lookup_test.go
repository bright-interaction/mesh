// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/retrieve"
)

func TestSearchWireExactTitleKeepsReceiptAndDirectWarning(t *testing.T) {
	// Keep this handler regression entirely local: a disposable vault has no
	// vectors or endpoint configuration, and HTTP selection bypasses the user's
	// optional subscription setup. Explicit FTS-only weights reproduce crowding
	// that the graph lane can otherwise mask.
	t.Setenv("MESH_RERANK_AGENT", "http")
	for _, key := range []string{"MESH_RERANK_ENDPOINT", "MESH_RERANK_MODEL", "MESH_EMBED_ENDPOINT", "MESH_EMBED_MODEL"} {
		t.Setenv(key, "")
	}
	t.Setenv("MESH_WEIGHT_FTS", "1")
	t.Setenv("MESH_WEIGHT_GRAPH", "0")
	t.Setenv("MESH_WEIGHT_VEC", "0")
	t.Setenv("MESH_FRESHNESS_HALFLIFE_DAYS", "0")

	// Mirror retrieve.receiptCompetition across the package boundary: the
	// warning is a stronger direct hit and links to a second, expansion-only
	// warning that must not displace the already-selected exact-title receipt.
	const query = "Orbit v1.2.3 activated for local reader only"
	s := serverWithNotes(t, map[string]string{
		"receipt.md": "---\nid: receipt\ntype: note\ntitle: " + query + "\nrelated: [warning]\n---\n# " + query + "\n" + strings.Repeat("Verified executable checksum and preserved configuration. ", 30),
		"warning.md": "---\nid: warning\ntype: gotcha\ntitle: Orbit v1.2.3 reader activation warning\nrelated: [older]\n---\n# Orbit v1.2.3 reader activation warning\n" + strings.Repeat(query+". ", 3) + "Keep the recovery procedure available.\n",
		"older.md":   "---\nid: older\ntype: gotcha\ntitle: Recovery procedure\n---\n# Recovery procedure\nKeep the previous executable until verification succeeds.\n",
	})
	_, r := s.snapshot()
	if fts, graph, vectors := r.Weights(); fts != 1 || graph != 0 || vectors != 0 {
		t.Fatalf("fixture weights are not FTS-only: %g/%g/%g", fts, graph, vectors)
	}
	if r.RerankActive() || r.VectorsActive() {
		t.Fatal("fixture must not enable a model provider")
	}

	const limit, budget = 2, 1200
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	call, err := batchE2ERequest(s, ctx, "mesh_search", map[string]any{"query": query, "limit": limit, "budget": budget})
	if err != nil {
		t.Fatal(err)
	}
	text, err := batchE2EText(call)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Cards  []retrieve.Card `json:"cards"`
		Tokens int             `json:"tokens"`
	}
	if err := json.Unmarshal([]byte(text), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Cards) != limit || response.Cards[0].NoteID != "receipt" || response.Cards[1].NoteID != "warning" {
		t.Fatalf("wire must retain the direct warning and exact-title receipt: %s", text)
	}
	if !response.Cards[1].Tier0 || response.Cards[0].Reason != "fts" || response.Cards[1].Reason != "fts" {
		t.Fatalf("direct warning provenance or protection changed: %s", text)
	}
	if response.Tokens <= 0 || response.Tokens > budget {
		t.Fatalf("reported tokens outside budget: %d", response.Tokens)
	}
	if got := retrieve.EstimateTokens(string(call.Result)); got > budget {
		t.Fatalf("complete MCP result exceeds budget: got %d, cap %d", got, budget)
	}
}
