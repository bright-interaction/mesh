// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/merge"
)

func TestLongConflictCanBeListedAndResolved(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, strings.Repeat("a", 247)+".md")
	sibling := merge.SiblingPath(base, time.Now(), "alice", []byte("mine"))
	if err := os.MkdirAll(filepath.Dir(sibling), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base, []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sibling, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return listConflicts(root, true) })
	if err != nil || !strings.Contains(out, filepath.Base(base)) {
		t.Fatalf("list lost base: %s %v", out, err)
	}
	cmd := conflictsResolveCmd()
	cmd.SetArgs([]string{sibling, root, "--keep-base"})
	if _, err := captureStdout(t, cmd.Execute); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sibling); !os.IsNotExist(err) {
		t.Fatalf("sibling remains: %v", err)
	}
	b, err := os.ReadFile(base)
	if err != nil || string(b) != "base" {
		t.Fatalf("base lost: %q %v", b, err)
	}
}
