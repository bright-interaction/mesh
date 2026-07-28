// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A code root is normally a SHARED working tree: several agent sessions check out their own
// branches in it, so whatever is on disk is whoever ran last. A note citing a file that
// exists on main but not on the currently checked-out branch is not rot, and neither is one
// citing a file added since the last code reindex.
//
// Before this, dead_ref trusted the code index alone and reported 25 findings on a live
// 900-note vault; 16 of them named files that were present in main the whole time. A check
// that is wrong two thirds of the time is one nobody reads, which is worse than not having
// it, because the 9 real findings were buried in the noise.
func TestDeadRefDoesNotFireForFilesGitStillKnows(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	run("init", "-q", "-b", "main")
	write("pkg/live.go", "package pkg\n")
	run("add", "-A")
	run("commit", "-qm", "add live.go")

	// Now simulate the shared-checkout situation: the file exists on main, but the working
	// tree has moved to another branch where it does not.
	run("checkout", "-q", "-b", "other")
	run("rm", "-q", "pkg/live.go")
	run("commit", "-qm", "remove on this branch only")

	if _, err := os.Stat(filepath.Join(root, "pkg/live.go")); err == nil {
		t.Fatal("setup wrong: the file should be absent from the working tree")
	}

	refs := map[string]bool{"pkg/live.go": true, "pkg/never-existed.go": true}
	found := existsUnderRoots([]string{root}, refs)

	if !found["pkg/live.go"] {
		t.Error("a file that main still has was treated as dead; the shared checkout being on " +
			"another branch is not rot")
	}
	// The check must stay useful: something git has never heard of is still dead.
	if found["pkg/never-existed.go"] {
		t.Error("a file no branch has ever contained was reported as existing, so dead_ref " +
			"would never fire again")
	}
}

// The filesystem alone must still be enough outside a git repo, or a non-git code root
// loses the check entirely.
func TestDeadRefUsesFilesystemOutsideGit(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "there.go"), []byte("package pkg\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	found := existsUnderRoots([]string{root}, map[string]bool{
		"pkg/there.go": true,
		"pkg/gone.go":  true,
	})
	if !found["pkg/there.go"] {
		t.Error("a file present on disk was reported missing")
	}
	if found["pkg/gone.go"] {
		t.Error("a file absent from disk was reported present")
	}
}
