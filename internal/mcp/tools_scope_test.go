// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
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

// seedCode indexes one source symbol so the code tools have something to return, and
// returns the symbol name to query for.
func seedCode(t *testing.T, s *Server) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x.go"), []byte("package x\n\nfunc UniqueWidgetMaker() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := index.ReindexCode(s.store, []string{root}, nil); err != nil {
		t.Fatal(err)
	}
	return "UniqueWidgetMaker"
}

func codeCount(t *testing.T, out any) int {
	t.Helper()
	res := toolJSON(t, out)
	if c, ok := res["count"].(float64); ok {
		return int(c)
	}
	return len(asList(res["symbols"]))
}

// TestCodeSearchScopeGate: the source-code index is unscoped (dev-scoped) content, so
// a scope-confined caller who cannot read the dev scope must get NO code symbols,
// while an unrestricted or dev-scope caller still does. This is the leak the audit
// flagged: code_search/neighbors predated scopes and took no ctx, bypassing the gate.
func TestCodeSearchScopeGate(t *testing.T) {
	s := scopedServer(t)
	q := seedCode(t, s)

	// Unrestricted (nil filter) finds the symbol.
	out, rerr := s.toolCodeSearch(context.Background(), json.RawMessage(`{"query":"`+q+`"}`))
	if rerr != nil {
		t.Fatalf("unrestricted code search: %v", rerr)
	}
	if n := codeCount(t, out); n < 1 {
		t.Fatalf("unrestricted code search = %d, want >=1", n)
	}

	// A dev-scope caller may read code (code is dev-scoped content).
	dev := WithScopeFilter(context.Background(), &ScopeFilter{AllowedRead: map[string]bool{"dev": true}})
	out, _ = s.toolCodeSearch(dev, json.RawMessage(`{"query":"`+q+`"}`))
	if n := codeCount(t, out); n < 1 {
		t.Fatalf("dev-scope code search = %d, want >=1", n)
	}

	// A non-dev scoped caller must NOT see code symbols.
	teamA := WithScopeFilter(context.Background(), &ScopeFilter{AllowedRead: map[string]bool{"team-a": true}})
	out, rerr = s.toolCodeSearch(teamA, json.RawMessage(`{"query":"`+q+`"}`))
	if rerr != nil {
		t.Fatalf("scoped code search: %v", rerr)
	}
	if n := codeCount(t, out); n != 0 {
		t.Fatalf("scoped code search leaked %d symbols across scope", n)
	}
}

// TestCodeNeighborsScopeGate: a non-dev scoped caller gets no call-graph neighbors.
func TestCodeNeighborsScopeGate(t *testing.T) {
	s := scopedServer(t)
	seedCode(t, s)

	teamA := WithScopeFilter(context.Background(), &ScopeFilter{AllowedRead: map[string]bool{"team-a": true}})
	out, rerr := s.toolCodeNeighbors(teamA, json.RawMessage(`{"id":"code:x/x.go#UniqueWidgetMaker"}`))
	if rerr != nil {
		t.Fatalf("scoped code neighbors: %v", rerr)
	}
	res := toolJSON(t, out)
	if len(asList(res["callers"])) != 0 || len(asList(res["callees"])) != 0 {
		t.Fatalf("scoped code neighbors leaked edges: %v", res)
	}
	if res["note"] != "the source-code index is not in your access scope" {
		t.Fatalf("scoped code neighbors missing deny note: %v", res)
	}
}

// TestReindexCodeCountsScopeGate: mesh_reindex must not report code corpus volume to a
// scope-confined caller (the aggregate-count leak the audit flagged). Code indexing is
// enabled via env so reindexCode actually returns counts.
func TestReindexCodeCountsScopeGate(t *testing.T) {
	s := scopedServer(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x.go"), []byte("package x\n\nfunc UniqueWidgetMaker() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MESH_CODE_INDEX", "1")
	t.Setenv("MESH_CODE_ROOTS", root)

	// A non-dev scoped caller must not learn the code corpus volume.
	teamA := WithScopeFilter(context.Background(), &ScopeFilter{AllowedRead: map[string]bool{"team-a": true}})
	out, rerr := s.toolReindex(teamA)
	if rerr != nil {
		t.Fatalf("scoped reindex: %v", rerr)
	}
	res := toolJSON(t, out)
	if _, ok := res["code_symbols"]; ok {
		t.Fatalf("scoped reindex leaked code counts: %v", res)
	}

	// An unrestricted caller still gets the counts.
	out, _ = s.toolReindex(context.Background())
	res = toolJSON(t, out)
	if _, ok := res["code_symbols"]; !ok {
		t.Fatalf("unrestricted reindex missing code counts: %v", res)
	}
}

// TestReindexNoteCountsScopeGate: mesh_reindex must report only the caller's readable
// graph view and omit the global reconcile deltas, so a scoped caller cannot learn the
// volume of notes outside their scope.
func TestReindexNoteCountsScopeGate(t *testing.T) {
	s := scopedServer(t) // note-a (team-a) + note-b (team-b)

	// Unrestricted: full graph totals + deltas present.
	out, rerr := s.toolReindex(context.Background())
	if rerr != nil {
		t.Fatalf("unrestricted reindex: %v", rerr)
	}
	full := toolJSON(t, out)
	if _, ok := full["added"]; !ok {
		t.Fatalf("unrestricted reindex missing deltas: %v", full)
	}
	globalNodes, ok := full["nodes"].(float64)
	if !ok || globalNodes < 2 {
		t.Fatalf("unrestricted nodes = %v, want >=2 (both notes)", full["nodes"])
	}

	// team-a: readable view only, no global deltas.
	teamA := WithScopeFilter(context.Background(), &ScopeFilter{AllowedRead: map[string]bool{"team-a": true}})
	out, rerr = s.toolReindex(teamA)
	if rerr != nil {
		t.Fatalf("scoped reindex: %v", rerr)
	}
	scoped := toolJSON(t, out)
	if _, ok := scoped["added"]; ok {
		t.Fatalf("scoped reindex leaked global deltas: %v", scoped)
	}
	sn, ok := scoped["nodes"].(float64)
	if !ok || sn < 1 {
		t.Fatalf("scoped nodes = %v, want >=1 (team-a sees its own note)", scoped["nodes"])
	}
	if sn >= globalNodes {
		t.Fatalf("scoped nodes=%v should be < global=%v (note-b's subgraph hidden)", sn, globalNodes)
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
