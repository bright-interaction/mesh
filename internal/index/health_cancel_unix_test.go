//go:build darwin || linux

// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestHealthStalledNoteReadCancelsWithoutPublishing(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pn := writeNote(t, dir, "stalled.md", "---\nid: stalled\ntype: note\n---\nBody\n")
	g, _ := BuildGraph([]*ParsedNote{pn})
	if _, err := s.IndexVault([]*ParsedNote{pn}, g); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "stalled.md")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Release any detached kernel open after cancellation. No filesystem
		// worker can write a report; only the joined Store worker can do that.
		fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			syscall.Close(fd)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	findings, err := s.scanHealthContext(ctx, dir, time.Now())
	if !errors.Is(err, context.DeadlineExceeded) || findings != nil {
		t.Fatalf("partial health: %v %v", findings, err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("stalled note blocked health cancellation")
	}
}

func TestHealthGitProbeHonorsCancellation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\nexec /bin/sleep 30\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	found, err := existsUnderRootsContext(ctx, []string{dir}, map[string]bool{"src/missing.go": true})
	if !errors.Is(err, context.DeadlineExceeded) || found != nil {
		t.Fatalf("accepted partial probe: %v %v", found, err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("git probe ignored cancellation")
	}
}
