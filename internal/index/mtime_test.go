// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMtimeFastPathDetectsChange: a normal edit (content + mtime move) is caught by
// the fast path.
func TestMtimeFastPathDetectsChange(t *testing.T) {
	dir := writeVault(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	live := NewLiveIndexer(s, dir)
	if _, err := live.Reconcile(true); err != nil { // seed
		t.Fatal(err)
	}
	apath := filepath.Join(dir, "a.md")
	if err := os.WriteFile(apath, []byte("---\nid: a\ntype: decision\nwhen: 2026-01-01\ndo: x\ndont: y\nwhy: changed\n---\n# A\nnew body #core\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(apath, future, future); err != nil {
		t.Fatal(err)
	}
	rec, err := live.Reconcile(false) // fast path
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Reindexed || rec.Changed != 1 {
		t.Fatalf("fast path should detect the changed file, got %+v", rec)
	}
}

// TestMtimeFastPathSkipsUnchangedButTickCatchesIt is the documented blind spot +
// its backstop: a mtime-preserving edit is invisible to the fast path (the mtime
// matches stored) but the authoritative periodic tick (full hash check) catches it.
func TestMtimeFastPathSkipsUnchangedButTickCatchesIt(t *testing.T) {
	dir := writeVault(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	live := NewLiveIndexer(s, dir)
	if _, err := live.Reconcile(true); err != nil { // seed
		t.Fatal(err)
	}
	var stored int64
	if err := s.readDB.QueryRow(`SELECT mtime FROM notes WHERE id='a'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	apath := filepath.Join(dir, "a.md")
	// Change the content but reset mtime to the stored value (a mtime-preserving edit).
	if err := os.WriteFile(apath, []byte("---\nid: a\ntype: decision\nwhen: 2026-01-01\ndo: x\ndont: y\nwhy: secretly changed\n---\n# A\nquietly different #core\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mt := time.Unix(stored, 0)
	if err := os.Chtimes(apath, mt, mt); err != nil {
		t.Fatal(err)
	}
	// Fast path: mtime unchanged -> file skipped -> the edit is invisible.
	rec, err := live.Reconcile(false)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Reindexed {
		t.Fatal("fast path should skip a file whose mtime did not move (the documented blind spot)")
	}
	// Authoritative tick: full hash check catches it.
	rec, err = live.Reconcile(true)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Reindexed || rec.Changed != 1 {
		t.Fatalf("authoritative reconcile must catch the mtime-preserving edit, got %+v", rec)
	}
}

// TestMtimeStoredCorrectlyOffRoot is the regression for the fileMtime bug: the
// stored mtime must be the file's real mtime even when the process CWD is not the
// vault root (the normal MCP case). The test's CWD is the package dir, and the
// vault is a temp dir elsewhere, so this exercises exactly that off-root path.
// Before the fix (fileMtime stat'd the vault-relative path) this stored 0.
func TestMtimeStoredCorrectlyOffRoot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "n1.md")
	if err := os.WriteFile(path, []byte("---\nid: n1\ntype: note\nwhen: 2026-01-01\n---\n# N1\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, want, want); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err := ReindexFull(s, dir); err != nil {
		t.Fatal(err)
	}

	var got int64
	if err := s.readDB.QueryRow(`SELECT mtime FROM notes WHERE id='n1'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want.Unix() {
		t.Fatalf("stored mtime = %d, want %d (the real file mtime). 0 means the off-root fileMtime bug regressed", got, want.Unix())
	}

	// mesh_changed_since must see it: a since just before the mtime returns the note,
	// a since just after returns nothing.
	since, err := s.ChangedSince(want.Unix() - 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(since) != 1 || since[0].ID != "n1" {
		t.Fatalf("ChangedSince(mtime-1) should return n1, got %+v", since)
	}
	after, err := s.ChangedSince(want.Unix() + 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("ChangedSince(mtime+1) should be empty, got %+v", after)
	}
}
