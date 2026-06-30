// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "decisions"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("decisions/sqlite.md", "---\nid: sqlite\ntype: decision\nwhen: 2026-01-01\ndo: x\ndont: y\nwhy: use modernc sqlite for storage\n---\n# Storage\n")
	write("note.md", "---\nid: note\ntype: note\nwhen: 2026-01-01\n---\n# Note\nmarketing copy\n")

	srv, err := NewServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

func call(t *testing.T, s *Server, method string, params any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(params)
	res, rerr := s.dispatch(context.Background(), request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: method, Params: raw})
	if rerr != nil {
		t.Fatalf("%s: rpc error %d %s", method, rerr.Code, rerr.Message)
	}
	m, _ := res.(map[string]any)
	return m
}

// toolText pulls the JSON text out of an MCP tool content result.
func toolText(t *testing.T, res map[string]any) map[string]any {
	t.Helper()
	content, ok := res["content"].([]map[string]any)
	if !ok || len(content) == 0 {
		t.Fatalf("no content in tool result: %v", res)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(content[0]["text"].(string)), &out); err != nil {
		t.Fatalf("tool text not json: %v", err)
	}
	return out
}

func TestInitializeAndToolsList(t *testing.T) {
	s := newTestServer(t)
	init := call(t, s, "initialize", map[string]any{})
	if init["protocolVersion"] != protocolVersion {
		t.Errorf("protocolVersion = %v", init["protocolVersion"])
	}
	if init["instructions"] == "" {
		t.Error("expected instructions (the contract)")
	}
	list := call(t, s, "tools/list", map[string]any{})
	tools, _ := list["tools"].([]map[string]any)
	if len(tools) != 14 {
		t.Errorf("expected 14 tools, got %d", len(tools))
	}
}

func TestToolSearchFused(t *testing.T) {
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
	first, _ := cards[0].(map[string]any)
	if first["NodeID"] != "note:sqlite" {
		t.Errorf("top card = %v, want note:sqlite", first["NodeID"])
	}
}

func TestToolWriteBackReindexes(t *testing.T) {
	s := newTestServer(t)
	res, rerr := s.dispatch(context.Background(), request{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call",
		Params: mustJSON(map[string]any{"name": "mesh_append_note", "arguments": map[string]any{
			"type": "gotcha", "title": "Vec extensions unavailable",
			"do": "use flat cosine", "dont": "depend on vec0", "why": "modernc has no C extensions",
		}}),
	})
	if rerr != nil {
		t.Fatalf("write rpc error: %v", rerr)
	}
	w := toolText(t, res.(map[string]any))
	if w["id"] != "vec-extensions-unavailable" {
		t.Errorf("write id = %v", w["id"])
	}
	// The new gotcha must be immediately retrievable (reload worked).
	sr, _ := s.dispatch(context.Background(), request{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call",
		Params: mustJSON(map[string]any{"name": "mesh_search", "arguments": map[string]any{"query": "vec extensions modernc"}}),
	})
	out := toolText(t, sr.(map[string]any))
	cards, _ := out["cards"].([]any)
	var found bool
	for _, c := range cards {
		if cm, _ := c.(map[string]any); cm["NodeID"] == "note:vec-extensions-unavailable" {
			found = true
		}
	}
	if !found {
		t.Errorf("written gotcha not retrievable after reload: %v", cards)
	}
}

func TestToolWriteRecordsProvenance(t *testing.T) {
	s := newTestServer(t)
	// initialize first so the server learns the calling agent's name.
	if _, rerr := s.dispatch(context.Background(), request{
		JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "initialize",
		Params: mustJSON(map[string]any{"clientInfo": map[string]any{"name": "claude-code"}}),
	}); rerr != nil {
		t.Fatalf("initialize: %v", rerr)
	}
	res, rerr := s.dispatch(context.Background(), request{
		JSONRPC: "2.0", ID: json.RawMessage(`2`), Method: "tools/call",
		Params: mustJSON(map[string]any{"name": "mesh_append_note", "arguments": map[string]any{
			"type": "decision", "title": "Prov check note",
			"do": "stamp provenance", "dont": "drop authorship", "why": "audit + lifecycle need it",
			"confidence": "high", "review_by": "2027-01-01",
		}}),
	})
	if rerr != nil {
		t.Fatalf("write rpc error: %v", rerr)
	}
	w := toolText(t, res.(map[string]any))
	b, err := os.ReadFile(w["path"].(string))
	if err != nil {
		t.Fatalf("read written note: %v", err)
	}
	body := string(b)
	for _, want := range []string{"agent: claude-code", "source: agent", "confidence: high", "review_by:", "2027-01-01"} {
		if !strings.Contains(body, want) {
			t.Errorf("written note missing %q:\n%s", want, body)
		}
	}
}

func TestHandleHTTPMatchesDispatch(t *testing.T) {
	s := newTestServer(t)
	ts := httptest.NewServer(http.HandlerFunc(s.HandleHTTP))
	defer ts.Close()

	body := mustJSON(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	resp, err := http.Post(ts.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
		Error *map[string]any `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Error != nil {
		t.Fatalf("rpc error: %v", *out.Error)
	}
	if len(out.Result.Tools) != 14 {
		t.Fatalf("tools over HTTP = %d, want 14", len(out.Result.Tools))
	}

	// A tools/call over HTTP returns the same shape as stdio.
	call := mustJSON(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": "mesh_search", "arguments": map[string]any{"query": "sqlite"}}})
	r2, err := http.Post(ts.URL, "application/json", bytes.NewReader(call))
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	if r2.StatusCode != 200 {
		t.Fatalf("tools/call over HTTP status = %d", r2.StatusCode)
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// TestReindexPicksUpDirectFileEdit covers the IDE/CLI collaboration loop: an agent
// edits a note file directly (not via mesh_append_note), so the running server is
// stale until mesh_reindex forces a re-read. After reindex the new note is queryable.
func TestReindexPicksUpDirectFileEdit(t *testing.T) {
	s := newTestServer(t)
	dir := s.vaultRoot

	// A brand-new note written straight to disk, as the Edit tool would.
	if err := os.WriteFile(filepath.Join(dir, "decisions", "hnsw.md"),
		[]byte("---\nid: hnsw\ntype: decision\nwhen: 2026-02-02\ndo: gate it\ndont: always build\nwhy: pure-go hnsw ann index for large vault retrieval\n---\n# HNSW\n"),
		0o644); err != nil {
		t.Fatal(err)
	}

	// Before reindex: the server has not seen the file.
	out := toolText(t, call(t, s, "tools/call", map[string]any{
		"name": "mesh_search", "arguments": map[string]any{"query": "hnsw ann index"}}))
	for _, c := range asCards(out) {
		if c["NodeID"] == "note:hnsw" {
			t.Fatal("server should be stale before mesh_reindex")
		}
	}

	// Reindex, then it must be found.
	r := toolText(t, call(t, s, "tools/call", map[string]any{"name": "mesh_reindex", "arguments": map[string]any{}}))
	if r["reindexed"] != true || r["added"].(float64) != 1 {
		t.Fatalf("reindex should report 1 added, got %v", r)
	}
	out = toolText(t, call(t, s, "tools/call", map[string]any{
		"name": "mesh_search", "arguments": map[string]any{"query": "hnsw ann index"}}))
	found := false
	for _, c := range asCards(out) {
		if c["NodeID"] == "note:hnsw" {
			found = true
		}
	}
	if !found {
		t.Fatal("note must be retrievable after mesh_reindex")
	}

	// Idempotent: a second reindex with no change reports nothing added.
	r2 := toolText(t, call(t, s, "tools/call", map[string]any{"name": "mesh_reindex", "arguments": map[string]any{}}))
	if r2["added"].(float64) != 0 || r2["changed"].(float64) != 0 || r2["removed"].(float64) != 0 {
		t.Fatalf("second reindex should be a no-op, got %v", r2)
	}
}

func asCards(out map[string]any) []map[string]any {
	raw, _ := out["cards"].([]any)
	var cards []map[string]any
	for _, c := range raw {
		if m, ok := c.(map[string]any); ok {
			cards = append(cards, m)
		}
	}
	return cards
}
