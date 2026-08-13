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

// Two README.md files in different folders, no frontmatter, so both resolve to the note id
// "readme". One of them cannot be indexed. Every command that reports on a vault has to
// SAY so and exit non-zero, because the operator's only other signal used to be a STALE
// verdict that no reindex could clear and whose suggested remedy (mesh migrate --apply)
// stamps the same id into both files and cements the collision.
func dupIDVaultCLI(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"a", "b"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeNote(t, filepath.Join(dir, "a"), "README.md", "# Readme A\nalpha content\n")
	writeNote(t, filepath.Join(dir, "b"), "README.md", "# Readme B\nbravo content\n")
	return dir
}

func TestDuplicateNoteIDIsReportedByEveryOperatorCommand(t *testing.T) {
	t.Run("mesh index names the file it left out and exits non-zero", func(t *testing.T) {
		dir := dupIDVaultCLI(t)
		out, err := runCLI(t, indexCmd(), dir)
		if err == nil {
			t.Errorf("mesh index exited 0 after leaving a note out of the index:\n%s", out)
		}
		if !strings.Contains(out, filepath.Join("b", "README.md")) {
			t.Errorf("mesh index must name the quarantined file:\n%s", out)
		}
		if !strings.Contains(out, "different id") {
			t.Errorf("mesh index must name the remedy:\n%s", out)
		}
	})

	t.Run("mesh init reports the note it could not index", func(t *testing.T) {
		dir := dupIDVaultCLI(t)
		out, err := runCLI(t, initCmd(), dir)
		if err == nil {
			t.Errorf("mesh init exited 0 on a vault where one of two notes is invisible:\n%s", out)
		}
		if !strings.Contains(out, filepath.Join("b", "README.md")) {
			t.Errorf("mesh init must name the file that is not in the index:\n%s", out)
		}
	})

	t.Run("mesh doctor blames the duplicate, not drift", func(t *testing.T) {
		dir := dupIDVaultCLI(t)
		if _, err := runCLI(t, indexCmd(), dir); err == nil {
			t.Fatal("mesh index should have reported the duplicate")
		}
		out, err := runCLI(t, doctorCmd(), dir)
		if err == nil {
			t.Errorf("doctor exited 0 with a note invisible to search:\n%s", out)
		}
		if !strings.Contains(out, "status: BROKEN") {
			t.Errorf("doctor must report BROKEN:\n%s", out)
		}
		// The whole point: the verdict is the duplicate, not the endless STALE that a
		// reindex cannot clear.
		if strings.Contains(out, "status: STALE") {
			t.Errorf("doctor still reports STALE, which no reindex can fix:\n%s", out)
		}
		if !strings.Contains(out, filepath.Join("b", "README.md")) {
			t.Errorf("doctor must name the offending file:\n%s", out)
		}
		// And a second index run must not change anything, i.e. the vault converges.
		if _, err := runCLI(t, indexCmd(), dir); err == nil {
			t.Error("mesh index should keep reporting the duplicate until it is fixed")
		}
		out2, _ := runCLI(t, doctorCmd(), dir)
		if strings.Contains(out2, "status: STALE") {
			t.Errorf("the index never converges: doctor still says STALE after a fresh index:\n%s", out2)
		}
	})

	t.Run("mesh health does not call the vault healthy", func(t *testing.T) {
		dir := dupIDVaultCLI(t)
		if _, err := runCLI(t, indexCmd(), dir); err == nil {
			t.Fatal("mesh index should have reported the duplicate")
		}
		out, err := runCLI(t, healthCmd(), dir)
		if err == nil {
			t.Errorf("health exited 0 with a quarantined note:\n%s", out)
		}
		if strings.Contains(out, "vault healthy") {
			t.Errorf("health called a vault with an invisible note healthy:\n%s", out)
		}
		if !strings.Contains(out, "duplicate-id") || !strings.Contains(out, filepath.Join("b", "README.md")) {
			t.Errorf("health must name the duplicate and the file:\n%s", out)
		}
	})

	t.Run("fixing the id clears every verdict", func(t *testing.T) {
		dir := dupIDVaultCLI(t)
		if _, err := runCLI(t, indexCmd(), dir); err == nil {
			t.Fatal("mesh index should have reported the duplicate")
		}
		// The remedy the messages give: a distinct id on one of the two.
		writeNote(t, filepath.Join(dir, "b"), "README.md",
			"---\nid: readme-b\ntype: note\nwhen: \"2026-08-13\"\n---\n# Readme B\nbravo content\n")
		if out, err := runCLI(t, indexCmd(), dir); err != nil {
			t.Fatalf("mesh index still fails after the remedy: %v\n%s", err, out)
		}
		// Play the owning writer, the way `mesh watch` or an elected `mesh mcp` does, so
		// the verdict under test is the duplicate-id one and nothing else. A vault with
		// no owner no longer fails doctor on its own, but it does change the printed
		// verdict line, and this case is about that line clearing completely.
		lock, err := index.AcquireOwnerLock(filepath.Join(dir, ".mesh"), "test owner", false)
		if err != nil {
			t.Fatalf("acquire owner lock: %v", err)
		}
		defer lock.Release()
		if out, err := runCLI(t, doctorCmd(), dir); err != nil {
			t.Fatalf("doctor still fails after the remedy: %v\n%s", err, out)
		}
		if out, err := runCLI(t, healthCmd(), dir); err != nil {
			t.Fatalf("health still fails after the remedy: %v\n%s", err, out)
		}
	})
}
