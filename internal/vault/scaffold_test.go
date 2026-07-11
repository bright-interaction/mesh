// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixedNow(t *testing.T) {
	t.Helper()
	orig := Now
	Now = func() time.Time { return time.Date(2026, 6, 16, 9, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { Now = orig })
}

func TestCreateNoteGotchaLeavesFlywheelTODOs(t *testing.T) {
	fixedNow(t)
	dir := t.TempDir()
	res, err := CreateNote(dir, NewNoteSpec{Type: TypeGotcha, Title: "Modernc cannot load sqlite-vec"})
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(filepath.Dir(res.Path)); got != "gotchas" {
		t.Errorf("placement = %s, want gotchas/", got)
	}
	if res.ID != "modernc-cannot-load-sqlite-vec" {
		t.Errorf("id = %q", res.ID)
	}
	if res.When != "2026-06-16" {
		t.Errorf("when = %q", res.When)
	}
	if len(res.TODOs) != 3 {
		t.Errorf("expected 3 flywheel TODOs, got %v", res.TODOs)
	}
	data, _ := os.ReadFile(res.Path)
	s := string(data)
	// yaml.v3 quotes date-like scalars so they stay strings; that round-trips fine.
	for _, want := range []string{
		"id: modernc-cannot-load-sqlite-vec",
		"type: gotcha",
		`when: "2026-06-16"`,
		`created: "2026-06-16"`,
		"do: TODO",
		"## Symptom",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("file missing %q\n---\n%s", want, s)
		}
	}
}

func TestCreateNoteCompleteDecisionLintsClean(t *testing.T) {
	fixedNow(t)
	dir := t.TempDir()
	res, err := CreateNote(dir, NewNoteSpec{
		Type:    TypeDecision,
		Title:   "Use Syncthing for sync",
		Do:      "use Syncthing plus a hub autocommit daemon",
		Dont:    "build a bespoke git hub first",
		Why:     "deletes the riskiest subsystem in the spec",
		Related: []string{"mesh", "[[graphify]]"},
		Tags:    []string{"#Sync", "sync"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.TODOs) != 0 {
		t.Fatalf("expected clean note, got TODOs %v", res.TODOs)
	}
	data, _ := os.ReadFile(res.Path)
	s := string(data)
	if !strings.Contains(s, "graphify") {
		t.Errorf("related link not normalized/rendered:\n%s", s)
	}
	if c := strings.Count(s, "- sync\n"); c != 1 {
		t.Errorf("tags not deduped/normalized: %d occurrences of sync\n%s", c, s)
	}
}

func TestCreateNoteCollisionSuffixes(t *testing.T) {
	fixedNow(t)
	dir := t.TempDir()
	a, err := CreateNote(dir, NewNoteSpec{Type: TypeNote, Title: "Same Title"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := CreateNote(dir, NewNoteSpec{Type: TypeNote, Title: "Same Title"})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatalf("expected unique ids, both %q", a.ID)
	}
	if b.ID != "same-title-2" {
		t.Errorf("collision id = %q, want same-title-2", b.ID)
	}
}
