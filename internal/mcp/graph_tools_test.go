// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
)

func serverWithNotes(t *testing.T, notes map[string]string) *Server {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range notes {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srv, err := NewServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

func toolCall(t *testing.T, s *Server, name string, args map[string]any) map[string]any {
	t.Helper()
	res, rerr := s.dispatch(context.Background(), request{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call",
		Params: mustJSON(map[string]any{"name": name, "arguments": args}),
	})
	if rerr != nil {
		t.Fatalf("%s: rpc %d %s", name, rerr.Code, rerr.Message)
	}
	return toolText(t, res.(map[string]any))
}

const (
	hubNote   = "---\nid: hub\ntype: note\nwhen: 2026-01-01\ntags: [core]\n---\n# Hub\nlinks [[alpha]] and [[beta]].\n"
	alphaNote = "---\nid: alpha\ntype: note\nwhen: 2026-01-01\n---\n# Alpha\nsee [[beta]].\n"
	betaNote  = "---\nid: beta\ntype: note\nwhen: 2026-01-01\n---\n# Beta\nleaf.\n"
)

func linkedVault(t *testing.T) *Server {
	return serverWithNotes(t, map[string]string{"hub.md": hubNote, "alpha.md": alphaNote, "beta.md": betaNote})
}

func TestNeighbors(t *testing.T) {
	s := linkedVault(t)

	out := toolCall(t, s, "mesh_neighbors", map[string]any{"id": "hub"})
	if center, _ := out["center"].(map[string]any); center["id"] != "hub" {
		t.Fatalf("center = %v", out["center"])
	}
	dir := map[string]string{}
	for _, n := range out["neighbors"].([]any) {
		m := n.(map[string]any)
		dir[m["id"].(string)] = m["direction"].(string)
	}
	if dir["alpha"] != "out" || dir["beta"] != "out" {
		t.Fatalf("hub should reference alpha+beta outbound: %v", dir)
	}
	if _, ok := dir["tag:core"]; !ok {
		t.Fatalf("hub's tag neighbor should appear: %v", dir)
	}

	// beta is a leaf: it sees hub + alpha inbound.
	bout := toolCall(t, s, "mesh_neighbors", map[string]any{"id": "beta"})
	ids := map[string]string{}
	for _, n := range bout["neighbors"].([]any) {
		m := n.(map[string]any)
		ids[m["id"].(string)] = m["direction"].(string)
	}
	if ids["hub"] != "in" || ids["alpha"] != "in" {
		t.Fatalf("beta neighbors should be hub+alpha inbound: %v", ids)
	}

	// Unknown id is a clean param error, not a panic or empty result.
	if _, rerr := s.dispatch(context.Background(), request{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call",
		Params: mustJSON(map[string]any{"name": "mesh_neighbors", "arguments": map[string]any{"id": "ghost"}}),
	}); rerr == nil {
		t.Fatal("unknown id should error")
	}
}

func TestNeighborsDepthExpands(t *testing.T) {
	// alpha -> beta (out) at depth 1; from alpha at depth 2 we also reach hub
	// (inbound to alpha) and beta is already seen. Depth 2 must be a superset of depth 1.
	s := linkedVault(t)
	d1 := len(toolCall(t, s, "mesh_neighbors", map[string]any{"id": "alpha", "depth": 1})["neighbors"].([]any))
	d2 := len(toolCall(t, s, "mesh_neighbors", map[string]any{"id": "alpha", "depth": 2})["neighbors"].([]any))
	if d2 < d1 {
		t.Fatalf("depth 2 (%d) should be >= depth 1 (%d)", d2, d1)
	}
}

func TestNeighborsTruncationReportsTotal(t *testing.T) {
	// A hub linking to more notes than the limit must report the true total + a
	// truncated flag (never silently present a trimmed view as complete), and the
	// kept neighbors must be the highest-degree ones, not the first-listed.
	notes := map[string]string{}
	var body strings.Builder
	body.WriteString("---\nid: bighub\ntype: note\nwhen: 2026-01-01\n---\n# Big Hub\n")
	for i := 0; i < 60; i++ {
		body.WriteString("[[leaf" + itoa3(i) + "]] ")
		notes["leaf"+itoa3(i)+".md"] = "---\nid: leaf" + itoa3(i) + "\ntype: note\nwhen: 2026-01-01\n---\n# Leaf\n"
	}
	notes["bighub.md"] = body.String()
	// Give leaf00 extra inbound edges so it is the highest-degree leaf and MUST
	// survive the cap (proves sort-before-truncate, not first-listed-wins).
	notes["fan1.md"] = "---\nid: fan1\ntype: note\nwhen: 2026-01-01\n---\n# Fan1\n[[leaf00]]\n"
	notes["fan2.md"] = "---\nid: fan2\ntype: note\nwhen: 2026-01-01\n---\n# Fan2\n[[leaf00]]\n"

	s := serverWithNotes(t, notes)
	out := toolCall(t, s, "mesh_neighbors", map[string]any{"id": "bighub"}) // default limit 50

	if out["truncated"] != true {
		t.Fatalf("a 60-neighbor hub at limit 50 must report truncated=true: %v", out["truncated"])
	}
	if total, _ := out["total"].(float64); int(total) < 60 {
		t.Fatalf("total should be >= 60 (the real neighborhood), got %v", out["total"])
	}
	ns := out["neighbors"].([]any)
	if len(ns) != 50 {
		t.Fatalf("count should be capped at 50, got %d", len(ns))
	}
	kept := map[string]bool{}
	for _, n := range ns {
		kept[n.(map[string]any)["id"].(string)] = true
	}
	if !kept["leaf00"] {
		t.Fatal("the highest-degree neighbor (leaf00) must survive truncation (sort-before-cap)")
	}
}

func itoa3(n int) string {
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

func TestCommunity(t *testing.T) {
	s := linkedVault(t)

	ov := toolCall(t, s, "mesh_community", map[string]any{})
	if comms, _ := ov["communities"].([]any); len(comms) == 0 {
		t.Fatal("overview should list at least one community")
	}

	one := toolCall(t, s, "mesh_community", map[string]any{"id": "hub"})
	ids := map[string]bool{}
	for _, m := range one["members"].([]any) {
		ids[m.(map[string]any)["id"].(string)] = true
	}
	// hub is in its own community's member list. (We do NOT assert alpha/beta are
	// here: this 3-note triangle has no real community structure - every partition
	// scores modularity 0 - so Louvain's grouping is valid either way. Louvain's
	// quality is proven on a structured graph in internal/graph/community_test.go.)
	if !ids["hub"] {
		t.Fatalf("hub's community must contain hub: %v", ids)
	}
	// Members are notes only (the tag is not a member).
	if ids["core"] {
		t.Fatal("a tag must not appear as a community member")
	}
}

func TestStatsResourceEmpty(t *testing.T) {
	s := newTestServer(t)
	res := call(t, s, "resources/read", map[string]any{"uri": "mesh://stats"})
	contents, _ := res["contents"].([]map[string]any)
	if len(contents) == 0 {
		t.Fatal("no resource contents")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(contents[0]["text"].(string)), &payload); err != nil {
		t.Fatal(err)
	}
	vecs, ok := payload["vectors"].(map[string]any)
	if !ok {
		t.Fatalf("mesh://stats missing vectors: %v", payload)
	}
	if vecs["active"] != false {
		t.Errorf("a vault with no vectors should report active=false, got %v", vecs["active"])
	}
	if vecs["total"].(float64) != 0 {
		t.Errorf("expected 0 vectors, got %v", vecs["total"])
	}
	if _, ok := payload["rerank"]; !ok {
		t.Error("stats should include a rerank block")
	}
}

func TestStatsResourceActiveAndFresh(t *testing.T) {
	t.Setenv("MESH_EMBED_ENDPOINT", "http://127.0.0.1:1/v1") // unreachable; Dim() probe -> 0 -> lenient
	t.Setenv("MESH_EMBED_MODEL", "test-model")
	s := newTestServer(t)
	// Seed a live vector for note:sqlite with the matching note_hash, then reload so
	// the retriever picks it up.
	h, _ := s.store.NoteRetrievalHash("note:sqlite")
	if err := s.store.ReplaceVectors("test-model", []index.VectorRow{
		{NodeID: "note:sqlite", ChunkIx: 0, Vec: []float32{1, 0, 0, 0}, NoteHash: h},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.reload(); err != nil {
		t.Fatal(err)
	}
	res := call(t, s, "resources/read", map[string]any{"uri": "mesh://stats"})
	contents, _ := res["contents"].([]map[string]any)
	var payload map[string]any
	if err := json.Unmarshal([]byte(contents[0]["text"].(string)), &payload); err != nil {
		t.Fatal(err)
	}
	vecs := payload["vectors"].(map[string]any)
	if vecs["active"] != true {
		t.Errorf("vectors should be active, got %v", vecs["active"])
	}
	if vecs["model"] != "test-model" {
		t.Errorf("model = %v, want test-model", vecs["model"])
	}
	if vecs["live"].(float64) != 1 || vecs["total"].(float64) != 1 {
		t.Errorf("expected 1 live / 1 total, got live=%v total=%v", vecs["live"], vecs["total"])
	}
	if vecs["fresh_pct"].(float64) != 100 {
		t.Errorf("fresh_pct = %v, want 100", vecs["fresh_pct"])
	}
}

func TestCommunityResource(t *testing.T) {
	s := linkedVault(t)
	res := call(t, s, "resources/read", map[string]any{"uri": "mesh://community"})
	contents, _ := res["contents"].([]map[string]any)
	if len(contents) == 0 {
		t.Fatal("no resource contents")
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(contents[0]["text"].(string)), &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["communities"]; !ok {
		t.Fatalf("mesh://community missing communities: %v", payload)
	}
}
