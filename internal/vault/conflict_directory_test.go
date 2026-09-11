// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConflictDirectoryRefusesSymlinkAndUnrelatedParent(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	base := filepath.Join(root, "note.md")
	dir := filepath.Join(root, "base-note.sync-conflict-base-v1")
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}
	if err := PrepareSiblingDirectory(base, filepath.Join(dir, "copy.md")); err == nil {
		t.Fatal("followed an overflow-directory symlink")
	}
	if err := PrepareSiblingDirectory(base, filepath.Join(outside, "copy.md")); err == nil {
		t.Fatal("accepted unrelated parent")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside changed: %v %v", entries, err)
	}
}
