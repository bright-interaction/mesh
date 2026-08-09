// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The health findings are derived state that only a writer may persist, and after the
// single-writer split the per-window MCP servers stopped being writers. They used to
// refresh note_health as a side effect of an agent calling mesh_health, and the web
// dashboard reads those rows and computes nothing. So the owning writer refreshes them
// now: it is the one process that is both long-lived and allowed to write.

// TestOwnerRefreshesHealthOnItsFirstPass: after the owner has reconciled once, a reader
// (the dashboard, another process entirely) has findings to show.
func TestOwnerRefreshesHealthOnItsFirstPass(t *testing.T) {
	dir := t.TempDir()
	// review_by in the past: an overdue finding, deterministic and dependency-free.
	if err := os.WriteFile(filepath.Join(dir, "stale.md"),
		[]byte("---\nid: stale\ntype: note\nwhen: 2026-01-01\nreview_by: 2020-01-01\n---\n# Stale\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := NewLiveIndexer(store, dir).Reconcile(true); err != nil {
		t.Fatal(err)
	}

	findings, err := store.ListHealth("overdue")
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].NoteID != "stale" {
		t.Fatalf("the owner reconciled but left no health findings for a reader to show: %+v", findings)
	}
}

// TestOwnerDoesNotRepeatTheHealthPassEveryTick: the pass reads every note file in the
// vault, and the owner reconciles every few seconds. Running it per tick would put a
// whole-vault walk on the write path that every write-back's bound depends on.
func TestOwnerDoesNotRepeatTheHealthPassEveryTick(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stale.md"),
		[]byte("---\nid: stale\ntype: note\nwhen: 2026-01-01\nreview_by: 2020-01-01\n---\n# Stale\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	live := NewLiveIndexer(store, dir)

	if _, err := live.Reconcile(true); err != nil {
		t.Fatal(err)
	}
	if overdueCount(t, store) != 1 {
		t.Fatal("the first pass should have recorded the overdue note")
	}

	// Fix the note. Whether the pass re-ran is then visible in the FINDINGS rather than in
	// a detected_at timestamp, which has one-second granularity and compares equal across
	// a whole test either way (the first version of this test asserted on it and could not
	// have failed).
	writeFresh := func(id string) {
		if err := os.WriteFile(filepath.Join(dir, id+".md"),
			[]byte("---\nid: "+id+"\ntype: note\nwhen: 2026-01-01\n---\n# "+id+"\nbody\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFresh("stale")
	if _, err := live.Reconcile(true); err != nil {
		t.Fatal(err)
	}
	if overdueCount(t, store) != 1 {
		t.Fatal("the health pass ran again on the very next reconcile; it is meant to run on its own " +
			"slow cadence, not to put a whole-vault walk on every tick of the write path")
	}

	// And when the cadence IS due it runs: same reconcile path, only the clock moved.
	live.mu.Lock()
	live.lastHealth = time.Now().Add(-2 * healthRefreshInterval)
	live.mu.Unlock()
	writeFresh("nudge") // real work for the reconcile to do
	if _, err := live.Reconcile(true); err != nil {
		t.Fatal(err)
	}
	if n := overdueCount(t, store); n != 0 {
		t.Fatalf("the fixed note is still reported as overdue (%d rows) after the interval passed, so what "+
			"a reader sees would freeze at whatever the owner found when it started", n)
	}
}

func overdueCount(t *testing.T, s *Store) int {
	t.Helper()
	f, err := s.ListHealth("overdue")
	if err != nil {
		t.Fatal(err)
	}
	return len(f)
}
