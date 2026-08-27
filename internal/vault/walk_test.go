// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type scriptedDirEntry struct {
	name string
	dir  bool
}

func (e scriptedDirEntry) Name() string               { return e.name }
func (e scriptedDirEntry) IsDir() bool                { return e.dir }
func (e scriptedDirEntry) Type() fs.FileMode          { return 0 }
func (e scriptedDirEntry) Info() (fs.FileInfo, error) { return nil, nil }

func TestWalkSkipsConflictSiblings(t *testing.T) {
	dir := t.TempDir()
	wr := func(name string) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("---\nid: x\n---\n# x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wr("note.md")
	wr("note.sync-conflict-20260616-bob-1a2b3c4d.md")
	wr("decisions/d.md")
	wr("decisions/d.sync-conflict-20260616-alice-deadbeef.md")

	files, err := Walk(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if IsConflictSibling(filepath.Base(f)) {
			t.Errorf("Walk returned a conflict sibling: %s", f)
		}
	}
	if len(files) != 2 {
		t.Errorf("expected 2 real notes, got %d: %v", len(files), files)
	}
}

func TestIsConflictSibling(t *testing.T) {
	yes := []string{"x.sync-conflict-20260616-bob-1a2b3c4d.md", "a/b.sync-conflict-20260101-u-00000000.md"}
	no := []string{"x.md", "notes/y.md", "sync-conflict.md", "x.conflict.md"}
	for _, n := range yes {
		if !IsConflictSibling(n) {
			t.Errorf("%q should be a conflict sibling", n)
		}
	}
	for _, n := range no {
		if IsConflictSibling(n) {
			t.Errorf("%q should NOT be a conflict sibling", n)
		}
	}
}

func TestWalkContextAlreadyCancelledDoesNotStartTraversal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	files, err := walkContext(ctx, t.TempDir(), func(string, fs.WalkDirFunc) error {
		called = true
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WalkContext error = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("WalkContext started filesystem traversal with an already-cancelled context")
	}
	if files != nil {
		t.Fatalf("cancelled walk returned a partial file set: %v", files)
	}
}

func TestWalkContextCancellationStopsTraversalAndDropsPartialResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	root := t.TempDir()
	attempted, accepted := 0, 0
	workerDone := make(chan struct{})
	walk := func(_ string, visit fs.WalkDirFunc) error {
		defer close(workerDone)
		entries := []struct {
			path  string
			entry fs.DirEntry
		}{
			{root, scriptedDirEntry{name: filepath.Base(root), dir: true}},
			{filepath.Join(root, "first.md"), scriptedDirEntry{name: "first.md"}},
			{filepath.Join(root, "second.md"), scriptedDirEntry{name: "second.md"}},
			{filepath.Join(root, "third.md"), scriptedDirEntry{name: "third.md"}},
		}
		for i, item := range entries {
			attempted++
			if err := visit(item.path, item.entry, nil); err != nil {
				return err
			}
			accepted++
			if i == 1 { // root + one file were visited; the next callback must stop.
				cancel()
			}
		}
		return nil
	}

	files, err := walkContext(ctx, root, walk)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WalkContext error = %v, want context.Canceled", err)
	}
	// walkContext is allowed to return before its isolated read-only traversal worker.
	// Join that worker before inspecting its private test counters.
	<-workerDone
	if attempted != 3 || accepted != 2 {
		t.Fatalf("traversal continued after cancellation: attempted=%d accepted=%d", attempted, accepted)
	}
	if files != nil {
		t.Fatalf("cancelled walk exposed its partial file set: %v", files)
	}
}

func TestWalkContextCancellationDoesNotWaitForStalledTraversal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	workerExited := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		paths, err := walkContext(ctx, "/vault", func(string, fs.WalkDirFunc) error {
			close(started)
			<-release // simulate WalkDir blocked between callbacks
			close(workerExited)
			return ctx.Err()
		})
		if paths != nil {
			done <- errors.New("cancelled walk returned partial paths")
			return
		}
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("walk did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WalkContext returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled walk waited for stalled WalkDir")
	}

	close(release)
	select {
	case <-workerExited:
	case <-time.After(2 * time.Second):
		t.Fatal("late read-only walk worker did not exit")
	}
}
