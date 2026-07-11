// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"os"
	"path/filepath"
	"testing"
)

// A note with broken YAML frontmatter used to be dropped from the index with zero
// signal (it vanished from search and the graph). ReindexFull must now record it so
// an operator can find the vanished note instead of it silently disappearing.
func TestReindexRecordsDroppedUnparseableNotes(t *testing.T) {
	dir := t.TempDir()

	good := "---\nid: good-note\ntype: gotcha\nwhen: \"2026-07-11\"\n---\n# A good note\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "good.md"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	// A colon-space in an unquoted value is invalid YAML: the documented cause that
	// silently dropped real notes for weeks (arachne, dockyard-secrets-bridge, ...).
	broken := "---\nid: broken-note\ntype: gotcha\nupdated: 2026-06-18 (post-mortem: root cause)\n---\n# A broken note\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "broken.md"), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	_, notes, err := ReindexFull(store, dir)
	if err != nil {
		t.Fatal(err)
	}

	var ids []string
	for _, pn := range notes {
		if pn.FM != nil {
			ids = append(ids, pn.FM.ID)
		}
	}
	if !contains(ids, "good-note") {
		t.Errorf("the valid note should be indexed, got ids %v", ids)
	}
	if contains(ids, "broken-note") {
		t.Errorf("the broken note should not have been indexed (its YAML is invalid), got ids %v", ids)
	}

	dropped := store.DroppedNotes()
	if len(dropped) != 1 {
		t.Fatalf("expected exactly 1 dropped note, got %d: %+v", len(dropped), dropped)
	}
	if dropped[0].Path != "broken.md" {
		t.Errorf("dropped path should be vault-relative %q, got %q", "broken.md", dropped[0].Path)
	}
	if dropped[0].Err == nil {
		t.Error("dropped note should carry the parse error")
	}

	// A subsequent clean reindex must clear the record (the vault now parses fully).
	if err := os.Remove(filepath.Join(dir, "broken.md")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReindexFull(store, dir); err != nil {
		t.Fatal(err)
	}
	if d := store.DroppedNotes(); len(d) != 0 {
		t.Errorf("dropped record should clear once the vault parses cleanly, got %+v", d)
	}
}
