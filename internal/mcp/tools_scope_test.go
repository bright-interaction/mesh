package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// scopedServer builds a server over a vault with two notes in distinct scopes, each
// carrying an overdue review_by so the health pass yields exactly one finding per
// scope. This isolates the scope read-gate on the delta + health tools.
func scopedServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.md", "---\nid: note-a\ntype: note\nscope: team-a\nreview_by: 2020-01-01\n---\n# A\nalpha\n")
	write("b.md", "---\nid: note-b\ntype: note\nscope: team-b\nreview_by: 2020-01-01\n---\n# B\nbravo\n")
	srv, err := NewServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

func toolJSON(t *testing.T, out any) map[string]any {
	t.Helper()
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("tool result not a map: %T", out)
	}
	return toolText(t, m)
}

// TestChangedSinceScopeFilter: a scoped caller's delta must never include a note id
// from a scope they cannot read (the leak the audit found: changed_since predated
// scopes and exposed id/path/mtime of every changed note).
func TestChangedSinceScopeFilter(t *testing.T) {
	s := scopedServer(t)

	out, rerr := s.toolChangedSince(context.Background(), json.RawMessage(`{"since":0}`))
	if rerr != nil {
		t.Fatalf("changed_since unrestricted: %v", rerr)
	}
	if got := changedIDs(t, out); len(got) != 2 {
		t.Fatalf("unrestricted changed_since = %v, want both notes", got)
	}

	teamA := WithScopeFilter(context.Background(), &ScopeFilter{AllowedRead: map[string]bool{"team-a": true}})
	out, rerr = s.toolChangedSince(teamA, json.RawMessage(`{"since":0}`))
	if rerr != nil {
		t.Fatalf("changed_since scoped: %v", rerr)
	}
	ids := changedIDs(t, out)
	if len(ids) != 1 || ids[0] != "note-a" {
		t.Fatalf("scoped changed_since = %v, want [note-a]", ids)
	}
}

// TestHealthScopeFilter: a scoped caller's health pass must not surface findings (or
// counts) for notes outside their scope.
func TestHealthScopeFilter(t *testing.T) {
	s := scopedServer(t)
	teamA := WithScopeFilter(context.Background(), &ScopeFilter{AllowedRead: map[string]bool{"team-a": true}})

	out, rerr := s.toolHealth(teamA, json.RawMessage(`{}`))
	if rerr != nil {
		t.Fatalf("health scoped: %v", rerr)
	}
	res := toolJSON(t, out)
	for _, f := range asList(res["findings"]) {
		fm, _ := f.(map[string]any)
		if fm["note_id"] != "note-a" {
			t.Fatalf("health leaked out-of-scope finding: %v", fm)
		}
	}
	counts, _ := res["counts"].(map[string]any)
	if counts["overdue"] != float64(1) {
		t.Fatalf("scoped overdue count = %v, want 1", counts["overdue"])
	}
}

func changedIDs(t *testing.T, out any) []string {
	t.Helper()
	res := toolJSON(t, out)
	var ids []string
	for _, r := range asList(res["changed"]) {
		m, _ := r.(map[string]any)
		ids = append(ids, m["id"].(string))
	}
	return ids
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}
