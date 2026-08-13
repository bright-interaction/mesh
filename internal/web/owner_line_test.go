// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
)

// TestUIStartupLineConsultsTheRealOwnerLock.
//
// `mesh ui` without --own-index printed "index: read-only; the owning writer (mesh watch
// / mesh sync --watch) indexes" on every start, without ever looking at the lock. On a
// vault with no owner.lock on disk, where `mesh doctor` says "owner: NONE" and every note
// the operator writes from then on stays out of search, the viewer still announced that
// somebody was indexing. That banner is what an operator reads INSTEAD of checking.
func TestUIStartupLineConsultsTheRealOwnerLock(t *testing.T) {
	t.Run("no lock on disk: say nothing is indexing", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".mesh"), 0o700); err != nil {
			t.Fatal(err)
		}
		line := indexOwnershipLine(dir, false)
		if strings.Contains(line, "the owning writer") && !strings.Contains(line, "NOTHING is indexing") {
			t.Fatalf("the viewer claimed an owning writer indexes a vault with no owner.lock:\n%s", line)
		}
		if !strings.Contains(line, "NOTHING is indexing this vault") {
			t.Errorf("the line must say plainly that nothing indexes this vault:\n%s", line)
		}
		// And it must carry the same remedy every other surface gives, so the operator is
		// not left with a diagnosis and no next command.
		if !strings.Contains(line, "no owning writer detected") {
			t.Errorf("the line must carry index.NoOwnerRemedy:\n%s", line)
		}
	})

	t.Run("a live owner is named, not guessed at", func(t *testing.T) {
		dir := t.TempDir()
		lock, err := index.AcquireOwnerLock(filepath.Join(dir, ".mesh"), "mesh watch", false)
		if err != nil {
			t.Fatalf("acquire owner lock: %v", err)
		}
		defer lock.Release()

		line := indexOwnershipLine(dir, false)
		if !strings.Contains(line, "mesh watch") {
			t.Errorf("the line must name the process that actually holds the claim:\n%s", line)
		}
		if strings.Contains(line, "NOTHING is indexing") {
			t.Errorf("a vault with a live owner must not be reported as unindexed:\n%s", line)
		}
	})

	t.Run("--own-index still says this process owns it", func(t *testing.T) {
		dir := t.TempDir()
		line := indexOwnershipLine(dir, true)
		if !strings.Contains(line, "OWNED by this process") {
			t.Errorf("--own-index line changed:\n%s", line)
		}
	})
}
