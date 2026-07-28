// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Some notes exist BECAUSE a file was deleted: a retirement record, a post-mortem about
// removing a service. Their paths are supposed to be gone, so dead_ref flags them forever
// and is right every time. Six such notes sat permanently red in the live vault, and a
// check with permanent known-good findings is one people learn to skim past, which is how
// the genuine findings underneath get missed.
func TestExpectDeadRefsSuppressesOnlyTheOptedInNote(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A file that IS indexed, so its directory counts as judgeable.
	write("live.md", "---\nid: live\ntype: note\ntitle: Live\n---\n\n# Live\ncites internal/handlers/present.go\n")
	// Records a deletion, so it opts out.
	write("retired.md", "---\nid: retired\ntype: gotcha\ntitle: Retired\nexpect_dead_refs: true\n---\n\n"+
		"# Retired\nthe service is gone, see internal/handlers/removed.go\n")
	// Does NOT opt out, so it must still be reported.
	write("stale.md", "---\nid: stale\ntype: gotcha\ntitle: Stale\n---\n\n"+
		"# Stale\nstill claims internal/handlers/alsogone.go\n")

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err := ReindexFull(s, dir); err != nil {
		t.Fatal(err)
	}
	// Seed a code index so dead_ref runs at all and internal/handlers counts as a
	// directory we index (an unindexed directory is deliberately never judged).
	if _, err := s.writeDB.Exec(
		`INSERT OR REPLACE INTO code_files(path,lang,package,mtime,retrieval_hash) VALUES(?,?,?,?,?)`,
		"repo/internal/handlers/present.go", "go", "handlers", time.Now().Unix(), "h"); err != nil {
		t.Fatal(err)
	}

	findings, err := s.ComputeHealth(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var flagged []string
	for _, f := range findings {
		if f.Issue == "dead_ref" {
			flagged = append(flagged, f.NoteID+":"+f.Detail)
		}
	}
	joined := strings.Join(flagged, " ")
	if strings.Contains(joined, "retired") {
		t.Errorf("a note that opted out was still reported: %v", flagged)
	}
	if !strings.Contains(joined, "stale") {
		t.Errorf("the opt-out must be per-note, but a note that did NOT opt out went unreported: %v", flagged)
	}
}
