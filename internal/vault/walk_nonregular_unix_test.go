//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWalkExcludesFIFOAndSymlinkMarkdownEntries(t *testing.T) {
	root := t.TempDir()
	realNote := filepath.Join(root, "real.md")
	if err := os.WriteFile(realNote, []byte("# real\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realNote, filepath.Join(root, "linked.md")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "blocked.md"), 0o600); err != nil {
		t.Fatal(err)
	}

	files, err := WalkContext(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != realNote {
		t.Fatalf("WalkContext returned %v, want only regular note %q", files, realNote)
	}
}
