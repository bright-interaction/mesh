// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
)

// The owning-writer election is right and stays. What it must not do is fail a vault
// that is fine.
//
// Making "no owning writer" reportable turned `mesh doctor` and `mesh status` into
// commands that exit 1 on a PRISTINE vault: `mesh init CIvault && mesh doctor CIvault`
// printed drift +0 +0 -0, lint 0 problems, then "status: NO OWNER" and exited 1. The
// README names `mesh doctor` as the command to put in CI and calls its exit code the
// contract, so the documented recipe could not pass on a healthy vault, and the only way
// to make CI green was to start a daemon inside the CI job.
//
// The contract these cases pin:
//   - missing owner, index in sync   -> NOTICE, exit 0 (the gap is still printed)
//   - missing owner, index drifted   -> FAILURE, exit non-zero (nothing will clear it)
//   - `mesh status` never fails on a missing owner; it reports counts.

// TestDoctorOnAPristineVaultExitsZero is the README's CI recipe, run verbatim.
func TestDoctorOnAPristineVaultExitsZero(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "CIvault")

	initOut, err := runCLI(t, initCmd(), dir)
	if err != nil {
		t.Fatalf("mesh init %s: %v\n%s", dir, err, initOut)
	}

	out, err := runCLI(t, doctorCmd(), dir)
	if err != nil {
		t.Fatalf("`mesh init && mesh doctor` failed on a freshly initialised vault: %v\n%s\n"+
			"This is the recipe the README tells the reader to gate CI on.", err, out)
	}
	// Reporting the gap is the half of the round-1 fix that must survive: the exit code
	// changed, the visibility did not.
	for _, want := range []string{"owner:  NONE", "no owning writer detected", "NOTICE: no owning writer"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor stopped reporting the missing owner (%q absent):\n%s", want, out)
		}
	}
	if strings.Contains(out, "status: healthy") {
		t.Errorf("doctor called an unowned vault healthy; the gap must be in the verdict line:\n%s", out)
	}
}

// TestDoctorFailsOnDriftWithNoOwner is the other half: the escalation. A vault that has
// already drifted with nothing running to catch it up is a real failure, and doctor must
// keep exiting non-zero on it.
func TestDoctorFailsOnDriftWithNoOwner(t *testing.T) {
	dir := t.TempDir()
	writeNote(t, dir, "good.md", goodNote)
	if out, err := runCLI(t, indexCmd(), dir); err != nil {
		t.Fatalf("mesh index: %v\n%s", err, out)
	}
	// Drift: a note on disk the index has never seen.
	writeNote(t, dir, "later.md",
		"---\nid: later-note\ntype: note\ntitle: Later\nwhen: \"2026-08-13\"\n---\n# Later\n\nBody.\n")

	out, err := runCLI(t, doctorCmd(), dir)
	if err == nil {
		t.Fatalf("doctor exited 0 on an index that has drifted with no owner to catch it up:\n%s", out)
	}
	if !strings.Contains(out, "status: STALE") {
		t.Errorf("the compound failure must still read as STALE:\n%s", out)
	}
	if !strings.Contains(out, "no owning writer is running to catch it up") {
		t.Errorf("the compound failure must name BOTH causes, or the operator runs mesh index and it drifts again:\n%s", out)
	}
}

// TestDoctorStillReportsDriftUnderALiveOwner keeps plain STALE distinguishable from the
// compound verdict, so the two branches cannot collapse into one.
func TestDoctorStillReportsDriftUnderALiveOwner(t *testing.T) {
	dir := t.TempDir()
	writeNote(t, dir, "good.md", goodNote)
	if out, err := runCLI(t, indexCmd(), dir); err != nil {
		t.Fatalf("mesh index: %v\n%s", err, out)
	}
	writeNote(t, dir, "later.md",
		"---\nid: later-note\ntype: note\ntitle: Later\nwhen: \"2026-08-13\"\n---\n# Later\n\nBody.\n")

	lock, err := index.AcquireOwnerLock(filepath.Join(dir, ".mesh"), "test owner", false)
	if err != nil {
		t.Fatalf("acquire owner lock: %v", err)
	}
	defer lock.Release()

	out, err := runCLI(t, doctorCmd(), dir)
	if err == nil {
		t.Fatalf("doctor exited 0 on a stale index:\n%s", out)
	}
	if !strings.Contains(out, "status: STALE - run mesh index") {
		t.Errorf("a stale index under a live owner needs one command, not two:\n%s", out)
	}
}

// TestStatusNeverFailsOnAMissingOwner. `mesh status` reports what the index holds and
// never computes drift, so it has nothing to escalate on. It printed correct counts and
// then exited 1.
func TestStatusNeverFailsOnAMissingOwner(t *testing.T) {
	dir := t.TempDir()
	writeNote(t, dir, "good.md", goodNote)
	if out, err := runCLI(t, indexCmd(), dir); err != nil {
		t.Fatalf("mesh index: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".mesh", index.OwnerLockName)); !os.IsNotExist(err) {
		t.Fatalf("this vault was supposed to be unowned: %v", err)
	}

	out, err := runCLI(t, statusCmd(), dir)
	if err != nil {
		t.Fatalf("mesh status failed on an unowned vault whose index is perfectly readable: %v\n%s", err, out)
	}
	if !strings.Contains(out, "owner:  NONE") {
		t.Errorf("mesh status stopped reporting the missing owner:\n%s", out)
	}
	if !strings.Contains(out, "notice: no owning writer") {
		t.Errorf("mesh status must still say what a missing owner costs:\n%s", out)
	}
}
