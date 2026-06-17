package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
	if len(tools) != 8 {
		t.Errorf("expected 8 tools, got %d", len(tools))
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

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
