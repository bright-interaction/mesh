// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"path/filepath"
	"testing"

	"github.com/bright-interaction/mesh/internal/vault"
)

// TestADuplicateDoesNotManufactureEndlessDrift (dupid_test.go) pins the indexer half of
// this: a vault with two README.md quarantines one of them, deliberately, and reports no
// drift for it. What it explicitly does NOT cover is the operator's next move.
//
// `mesh doctor` names `mesh migrate --apply` as the remedy for a duplicate id. Migrate
// synthesized each id from its own file's basename with no notion of the rest of the
// vault, so both README.md were stamped `id: readme` and the collision stopped being a
// recoverable accident and became a fact on disk: one of the two notes is permanently
// unretrievable, and the tool the operator was pointed at is what put it there.
//
// This is the loop end to end. Migrate the vault the way the CLI does, reindex, and
// require that BOTH notes are now indexed, nothing is quarantined, and doctor reports no
// drift on repeated passes.
func TestMigrateConvergesAVaultWithDuplicateIDs(t *testing.T) {
	dir := dupIDVault(t)
	write(t, dir, "a/README.md", "# Readme A\nalpha content\n")
	write(t, dir, "b/README.md", "# Readme B\nbravo content\n")

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Before: the collision is live, one file is quarantined.
	if _, _, err := ReindexFull(s, dir); err != nil {
		t.Fatal(err)
	}
	if before := mustDropped(t, s); len(before) != 1 {
		t.Fatalf("precondition: exactly one file should be quarantined before the migration, got %v", droppedPaths(before))
	}

	// The remedy, exactly as `mesh migrate --apply` runs it: one Migration for the pass,
	// every file the walker yields, in walk order.
	files, err := vault.Walk(dir)
	if err != nil {
		t.Fatal(err)
	}
	m, err := vault.NewMigration(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if _, err := m.File(f, false); err != nil {
			t.Fatalf("migrate %s: %v", f, err)
		}
	}

	aID := vault.NoteIDForFile(filepath.Join(dir, "a", "README.md"))
	bID := vault.NoteIDForFile(filepath.Join(dir, "b", "README.md"))
	if aID == bID {
		t.Fatalf("the remedy cemented the collision: both README.md now declare id %q on disk, "+
			"so one of the two notes can never be retrieved again", aID)
	}

	// After: both notes index, nothing is dropped, and doctor converges. Twice, because a
	// second pass over an unchanged vault is what the operator runs to confirm the fix.
	for pass := 1; pass <= 2; pass++ {
		if _, _, err := ReindexFull(s, dir); err != nil {
			t.Fatal(err)
		}
		if dropped := mustDropped(t, s); len(dropped) != 0 {
			t.Fatalf("pass %d: the migrated vault still quarantines %v", pass, droppedPaths(dropped))
		}
		for _, id := range []string{aID, bID} {
			notePath, nodePath := dupNoteRow(t, s, id)
			if notePath != nodePath {
				t.Errorf("pass %d: id %q has notes.path %q but graph note_path %q", pass, id, notePath, nodePath)
			}
		}
		d, derr := s.DriftReport(dir)
		if derr != nil {
			t.Fatal(derr)
		}
		if d.Any() {
			t.Fatalf("pass %d: mesh doctor still reports drift after the migration (+%v ~%v -%v), "+
				"so it prints STALE forever over an index no reindex can change", pass, d.Added, d.Changed, d.Removed)
		}
	}
}
