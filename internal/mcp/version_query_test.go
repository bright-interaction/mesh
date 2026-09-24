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

func TestSearchWireDistinguishesRequestedPatchVersion(t *testing.T) {
	// A fresh disposable vault has no endpoint configuration or stored vectors.
	// Explicit HTTP selection bypasses user-local subscription configuration.
	t.Setenv("MESH_RERANK_AGENT", "http")
	for _, key := range []string{"MESH_RERANK_ENDPOINT", "MESH_RERANK_MODEL", "MESH_EMBED_ENDPOINT", "MESH_EMBED_MODEL"} {
		t.Setenv(key, "")
	}
	notes := map[string]string{}
	for i, id := range []string{"a-old", "z-requested"} {
		title := fmt.Sprintf("Mesh v0.41.%d activated for local MCP reader only", i+1)
		notes[id+".md"] = fmt.Sprintf("---\nid: %s\ntitle: %s\ntype: note\n---\n# %s\n", id, title, title)
	}
	s := serverWithNotes(t, notes)
	for _, limit := range []int{1, 2} {
		call, err := batchE2ERequest(s, context.Background(), "mesh_search", map[string]any{"query": "Mesh v0.41.2 activated for local MCP reader only", "limit": limit, "budget": 1200})
		if err != nil {
			t.Fatal(err)
		}
		text, err := batchE2EText(call)
		if err != nil {
			t.Fatal(err)
		}
		var response struct {
			Cards []retrieve.Card `json:"cards"`
		}
		if err := json.Unmarshal([]byte(text), &response); err != nil {
			t.Fatal(err)
		}
		if len(response.Cards) != limit || response.Cards[0].NoteID != "z-requested" {
			t.Fatalf("limit%d wire response lost version distinction: %s", limit, text)
		}
		if got := retrieve.EstimateTokens(string(call.Result)); got > 1200 {
			t.Fatalf("wire result budget exceeded: %d", got)
		}
	}
}
