// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The two passes that still wrote from a read-only window (step 3 of the single-writer
// split): the health pass and the code-index refresh. Both used to fail the whole tool
// call with ErrReadOnly, and neither may be "fixed" by serving whatever the owner last
// persisted, because on a vault whose owner never runs the pass that is nothing at all.

// TestHealthComputesWithoutWritingOnAReadOnlyServer: mesh_health must return the same
// findings it always did AND leave note_health untouched. The count check is what makes
// this a real assertion: if the tool were answering out of the persisted table, an empty
// table would give an empty answer, and if it were persisting, the table would not be
// empty.
func TestHealthComputesWithoutWritingOnAReadOnlyServer(t *testing.T) {
	s := scopedServer(t) // two notes, both with an overdue review_by

	if !s.store.ReadOnly() {
		t.Fatal("this test is about the read-only path; the fixture stopped being one")
	}
	before, err := s.store.Count("note_health")
	if err != nil {
		t.Fatal(err)
	}
	if before != 0 {
		t.Fatalf("fixture already has %d health rows, so this test cannot tell computed from persisted", before)
	}

	out, rerr := s.toolHealth(WithLocalOperator(context.Background()), json.RawMessage(`{}`))
	if rerr != nil {
		t.Fatalf("mesh_health on a read-only server: %v", rerr)
	}
	res := toolJSON(t, out)
	counts, _ := res["counts"].(map[string]any)
	if counts["overdue"] != float64(2) {
		t.Fatalf("health computed %v overdue, want 2 (both fixture notes are overdue)", counts["overdue"])
	}
	ids := map[string]bool{}
	for _, f := range asList(res["findings"]) {
		fm, _ := f.(map[string]any)
		ids[fm["note_id"].(string)] = true
	}
	if !ids["note-a"] || !ids["note-b"] {
		t.Fatalf("health findings = %v, want both notes", ids)
	}

	after, err := s.store.Count("note_health")
	if err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Fatalf("a read-only server persisted %d health rows; recording the pass belongs to the owning writer", after)
	}
}

// TestHealthFiltersByIssueOnAReadOnlyServer pins the argument the read-only path has to
// re-implement (the writable one gets it from SQL): counts stay global, findings narrow.
func TestHealthFiltersByIssueOnAReadOnlyServer(t *testing.T) {
	s := scopedServer(t)

	out, rerr := s.toolHealth(WithLocalOperator(context.Background()), json.RawMessage(`{"issue":"dead_ref"}`))
	if rerr != nil {
		t.Fatalf("mesh_health: %v", rerr)
	}
	res := toolJSON(t, out)
	if n := len(asList(res["findings"])); n != 0 {
		t.Fatalf("issue=dead_ref returned %d findings, want 0 (the fixture only has overdue ones)", n)
	}
	counts, _ := res["counts"].(map[string]any)
	if counts["overdue"] != float64(2) {
		t.Fatalf("the issue filter narrowed the COUNTS too (%v); it must only narrow the findings", counts)
	}
}

// TestReindexReportsCodeIndexTotalsAndItsOwner: mesh_reindex cannot refresh the code
// index from a read-only window, so it reports what the index it is serving holds and
// says who does refresh it. Returning nothing would leave an agent believing one
// mesh_reindex covered code too.
func TestReindexReportsCodeIndexTotalsAndItsOwner(t *testing.T) {
	s := newTestServer(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x.go"), []byte("package x\n\nfunc UniqueWidgetMaker() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedCodeIndex(t, s.vaultRoot, root)
	t.Setenv("MESH_CODE_INDEX", "1")
	t.Setenv("MESH_CODE_ROOTS", root)

	out, rerr := s.toolReindex(WithLocalOperator(context.Background()))
	if rerr != nil {
		t.Fatalf("mesh_reindex: %v", rerr)
	}
	res := toolJSON(t, out)
	if res["code_symbols"] != float64(1) {
		t.Fatalf("code_symbols = %v, want the 1 symbol the owner indexed", res["code_symbols"])
	}
	note, _ := res["code_index"].(string)
	if note == "" {
		t.Fatal("a read-only reindex reported code counts as if it had just refreshed them")
	}
	if !containsStr(note, "mesh code reindex") {
		t.Fatalf("the code-index note must name something that CAN refresh it, got: %q", note)
	}
}

// TestReindexWaitsForTheOwnerBeforeReportingSuccess is the honesty property of
// mesh_reindex under the split. The contract handed to every agent says "if you edited
// note files directly, call mesh_reindex to make those edits queryable now". This server
// cannot index, so the only way that sentence stays true is by waiting for the owner.
// Calling immediately after the write is the case that matters: the owner is still inside
// its debounce, so a bare snapshot swap would return success with nothing to show.
func TestReindexWaitsForTheOwnerBeforeReportingSuccess(t *testing.T) {
	s := newTestServer(t)
	startOwner(t, s.vaultRoot)

	if err := os.WriteFile(filepath.Join(s.vaultRoot, "decisions", "wait-for-owner.md"),
		[]byte("---\nid: wait-for-owner\ntype: decision\nwhen: 2026-02-02\ndo: wait\ndont: guess\nwhy: reindex must not report success before the owner has the note\n---\n# Wait\n"),
		0o644); err != nil {
		t.Fatal(err)
	}

	res := toolJSON(t, mustReindex(t, s))
	if res["index_stale"] == true {
		t.Fatalf("reindex reported a stale index with the owner running: %v", res)
	}
	if res["added"] != float64(1) {
		t.Fatalf("reindex reported %v added, want the 1 note written just before it", res["added"])
	}
	// The claim the tool makes about itself, checked against the index it just refreshed.
	if _, err := s.store.NotePath("wait-for-owner"); err != nil {
		t.Fatalf("mesh_reindex returned success but the note is not in the index: %v", err)
	}
}

// TestReindexWithNoOwnerFailsLoudly is the same bargain write-back makes: never hang,
// never claim success, and never send the agent somewhere that cannot help. There is no
// duplicate-note hazard here (nothing was written), but there IS a worse one: an agent
// that believes its edits are queryable will read stale answers and act on them.
func TestReindexWithNoOwnerFailsLoudly(t *testing.T) {
	s := newTestServer(t)
	s.ownerIndexTimeout = 300 * time.Millisecond // no owner will ever land it; do not wait 10s

	if err := os.WriteFile(filepath.Join(s.vaultRoot, "decisions", "no-owner.md"),
		[]byte("---\nid: no-owner\ntype: decision\nwhen: 2026-02-02\ndo: x\ndont: y\nwhy: nobody is indexing this\n---\n# No owner\n"),
		0o644); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	res := toolJSON(t, mustReindex(t, s))
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("reindex hung for %s with no owner; it must fail on a bound", elapsed)
	}
	if res["index_stale"] != true || res["owner_down"] != true {
		t.Fatalf("a reindex that never saw the owner catch up must report index_stale AND owner_down, got: %v", res)
	}
	if res["added"] != float64(0) {
		t.Fatalf("nothing became queryable, so nothing may be counted as added: %v", res)
	}
	warning, _ := res["warning"].(string)
	if !containsStr(warning, "mesh watch") {
		t.Fatalf("the owner-down warning must name what to start, got: %q", warning)
	}
}

// TestReindexIsNotBlockedByAQuarantinedNote is the trap in measuring drift to decide
// whether the owner has caught up. A duplicate-id quarantine parses fine but is
// deliberately absent from the notes table, so it reads as drift the owner will NEVER
// absorb. Counting it makes every mesh_reindex on such a vault wait out the whole bound
// and then report a perfectly healthy owner as down, forever.
func TestReindexIsNotBlockedByAQuarantinedNote(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "decisions"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeNote(t, filepath.Join(dir, "decisions", "seed.md"), "seed")
	writeNote(t, filepath.Join(dir, "decisions", "a.md"), "twin")
	writeNote(t, filepath.Join(dir, "decisions", "b.md"), "twin") // same id: one gets quarantined

	seedIndex(t, dir)
	srv, err := NewServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.WaitReady(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	srv.ownerIndexTimeout = 2 * time.Second // a full-bound wait here would be the bug
	startOwner(t, dir)

	start := time.Now()
	res := toolJSON(t, mustReindex(t, srv))
	elapsed := time.Since(start)

	if res["index_stale"] == true || res["owner_down"] == true {
		t.Fatalf("a quarantined duplicate made a healthy owner read as down: %v", res)
	}
	if elapsed >= srv.ownerIndexTimeout {
		t.Fatalf("reindex waited %s (the whole bound) on drift the owner can never absorb", elapsed)
	}
	// And the note that needs a human is still reported, by the surface that exists for it.
	out, rerr := srv.toolHealth(WithLocalOperator(context.Background()), json.RawMessage(`{"issue":"duplicate-id"}`))
	if rerr != nil {
		t.Fatalf("mesh_health: %v", rerr)
	}
	if n := len(asList(toolJSON(t, out)["findings"])); n != 1 {
		t.Fatalf("mesh_health reported %d duplicate-id findings, want the 1 quarantined note", n)
	}
}

// TestRefreshReportsNothingWhenNothingChanged: a refresh always swaps a graph in, but it
// must not claim a pass it did not need. The watcher logs a line whenever a reconcile
// reports Reindexed, and on a read-only server every periodic tick is a refresh, so
// reporting true unconditionally means a line every tick forever, which buries the ticks
// that actually brought something in.
func TestRefreshReportsNothingWhenNothingChanged(t *testing.T) {
	s := newTestServer(t)

	rec, err := s.reconcileOnce(true)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Reindexed || rec.Any() {
		t.Fatalf("an idle tick reported a reindex: %+v", rec)
	}

	// A note the owner lands between two ticks IS a change, and must be reported.
	seedNote(t, s.vaultRoot, "between-ticks")
	rec, err = s.reconcileOnce(true)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Reindexed || rec.Added != 1 {
		t.Fatalf("a tick that picked up one new note reported %+v", rec)
	}
}

// seedNote writes a note and has the owner-of-record (a one-shot writable store, like
// `mesh index`) put it in the index, without a watcher: the point is what the read-only
// server sees on its next refresh, not how fast the note got there.
func seedNote(t *testing.T, vaultRoot, id string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(vaultRoot, "decisions", id+".md"),
		[]byte("---\nid: "+id+"\ntype: decision\nwhen: 2026-02-02\ndo: x\ndont: y\nwhy: seeded between two watcher ticks\n---\n# "+id+"\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	seedIndex(t, vaultRoot)
}

func mustReindex(t *testing.T, s *Server) any {
	t.Helper()
	out, rerr := s.toolReindex(WithLocalOperator(context.Background()))
	if rerr != nil {
		t.Fatalf("mesh_reindex: %v", rerr)
	}
	return out
}
