// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Dropped notes used to live only in the memory of the process that indexed. That was
// fine while every server indexed its own vault. Under the single-writer split the
// process that INDEXES is no longer the process that REPORTS: every `mesh mcp` window is
// read-only, so its own record of what a pass dropped is permanently empty, and
// mesh_health would call a vault clean by virtue of never having looked.

// TestDroppedNotesReachAReaderInAnotherProcess: what the owning writer dropped must be
// visible to a read-only store opened against the same index.
func TestDroppedNotesReachAReaderInAnotherProcess(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("good.md", "---\nid: good\ntype: note\nwhen: 2026-01-01\n---\n# Good\nbody\n")
	// A colon-space in an unquoted value: invalid YAML, the documented cause.
	write("broken.md", "---\nid: broken\ntype: note\nupdated: 2026-06-18 (post-mortem: root cause)\n---\n# Broken\n")

	owner, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReindexFull(owner, dir); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	dropped := mustDropped(t, reader)
	if len(dropped) != 1 {
		t.Fatalf("a read-only reader saw %d dropped notes, want the 1 the owner recorded: %+v", len(dropped), dropped)
	}
	if dropped[0].Path != "broken.md" {
		t.Errorf("dropped path should be vault-relative %q, got %q", "broken.md", dropped[0].Path)
	}
	if dropped[0].Err == nil || dropped[0].Err.Error() == "" {
		t.Error("the drop crossed the boundary without the error that makes it actionable")
	}
}

// TestQuarantineSentinelSurvivesTheProcessBoundary: mesh_health branches on
// errors.Is(err, ErrDuplicateNoteID) to tell "fix the frontmatter" from "two notes claim
// one id", and those have different remedies. A sentinel does not survive a round trip
// through SQLite on its own, so it is rehydrated deliberately; without that, every
// quarantined duplicate would be reported to the operator as unparseable YAML.
func TestQuarantineSentinelSurvivesTheProcessBoundary(t *testing.T) {
	dir := writeVault(t)
	owner, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	live := NewLiveIndexer(owner, dir)
	if _, err := live.Reconcile(true); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "dup.md", "---\nid: b\ntype: note\nwhen: 2026-01-01\n---\n# Dup\nclashing id\n")
	if _, err := live.Reconcile(true); err != nil {
		t.Fatal(err)
	}
	inProcess := mustDropped(t, owner)
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	dropped := mustDropped(t, reader)
	if len(dropped) != 1 || dropped[0].Path != "dup.md" {
		t.Fatalf("the reader must see the quarantined file, got %+v", dropped)
	}
	if !errors.Is(dropped[0].Err, ErrDuplicateNoteID) {
		t.Fatalf("the duplicate-id sentinel did not survive the boundary, so a quarantine reads as "+
			"unparseable frontmatter and the operator is told the wrong remedy: %v", dropped[0].Err)
	}
	// Same message, not merely the same class: the detail names the file that already
	// claims the id, and that is the actionable half.
	if got, want := dropped[0].Err.Error(), inProcess[0].Err.Error(); got != want {
		t.Errorf("the message changed crossing the boundary:\n got %q\nwant %q", got, want)
	}
}

// TestDroppedRecordClearsForAReaderToo: the other direction. A fixed note must stop being
// reported, or the operator is sent after a file that is already fine.
func TestDroppedRecordClearsForAReaderToo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "broken.md"),
		[]byte("---\nid: broken\ntype: note\nupdated: 2026-06-18 (post-mortem: root cause)\n---\n# Broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	owner, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReindexFull(owner, dir); err != nil {
		t.Fatal(err)
	}

	reader, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if len(mustDropped(t, reader)) != 1 {
		t.Fatal("the reader should see the broken note before it is fixed")
	}

	// Fix it, and reindex. The set shrinks with no NEW entry, which is the case a
	// "persist only when something new appeared" check would miss.
	if err := os.WriteFile(filepath.Join(dir, "broken.md"),
		[]byte("---\nid: broken\ntype: note\nwhen: 2026-01-01\n---\n# Fixed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReindexFull(owner, dir); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if d := mustDropped(t, reader); len(d) != 0 {
		t.Errorf("the reader still reports a note that has been fixed: %+v", d)
	}
}
