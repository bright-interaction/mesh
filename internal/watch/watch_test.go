// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package watch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// waitCall blocks for a reconcile signal, failing the test on timeout.
func waitCall(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestRunReconcilesOnChange(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("# A\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	calls := make(chan struct{}, 64)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() {
		runDone <- Run(ctx, Options{
			Root:      dir,
			Debounce:  20 * time.Millisecond,
			Reconcile: 0, // isolate the fsnotify path
			OnReindex: func(authoritative bool) (Result, error) {
				calls <- struct{}{}
				return Result{Reindexed: true, Added: 1}, nil
			},
		})
	}()

	// Reconcile runs once at startup, with watches already in place.
	waitCall(t, calls, "startup reconcile")

	// Editing an existing note reindexes.
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("# A changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitCall(t, calls, "reconcile after edit")

	// Creating a new note reindexes.
	if err := os.WriteFile(filepath.Join(dir, "b.md"), []byte("# B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitCall(t, calls, "reconcile after create")

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run returned error on cancel: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestRunPeriodicReconcile(t *testing.T) {
	dir := t.TempDir()
	calls := make(chan struct{}, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, Options{
		Root:      dir,
		Debounce:  time.Hour, // make the event path irrelevant
		Reconcile: 40 * time.Millisecond,
		OnReindex: func(authoritative bool) (Result, error) {
			calls <- struct{}{}
			return Result{}, nil // no drift: the safety tick still fires the check
		},
	})

	// Startup reconcile plus at least one periodic tick prove the safety net runs
	// even with no file events at all.
	waitCall(t, calls, "startup reconcile")
	waitCall(t, calls, "first periodic reconcile")
	waitCall(t, calls, "second periodic reconcile")
}

// TestPeriodicTickIsCheapUntilTheFullInterval pins the split that keeps an idle laptop
// cool. The safety tick has to stay frequent (a note that missed its file event must be
// indexed before a reader gives up at the write-back bound), but only the occasional pass
// may be authoritative, because that one parses and content-hashes every note in the
// vault. Before the split, every tick did the expensive half: three idle daemons held
// ~12% of a core between them on a 1216-note vault, forever, with nothing changing.
func TestPeriodicTickIsCheapUntilTheFullInterval(t *testing.T) {
	dir := t.TempDir()
	type call struct{ authoritative bool }
	calls := make(chan call, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, Options{
		Root:          dir,
		Debounce:      time.Hour, // isolate the tick from the event path
		Reconcile:     20 * time.Millisecond,
		FullReconcile: 400 * time.Millisecond,
		OnReindex: func(authoritative bool) (Result, error) {
			calls <- call{authoritative}
			return Result{}, nil
		},
	})

	// Startup is always authoritative: nothing is known about what changed while the
	// watcher was not running.
	select {
	case c := <-calls:
		if !c.authoritative {
			t.Fatal("startup reconcile must be authoritative")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the startup reconcile")
	}

	// Every tick inside the full-reconcile window takes the cheap path. With a 20ms tick
	// against a 400ms window there are many of them, so a single authoritative pass here
	// would mean the expensive half is still running on the tick's cadence.
	deadline := time.After(300 * time.Millisecond)
	cheap := 0
	for done := false; !done; {
		select {
		case c := <-calls:
			if c.authoritative {
				t.Fatalf("tick %d inside the full-reconcile window was authoritative", cheap+1)
			}
			cheap++
		case <-deadline:
			done = true
		}
	}
	if cheap < 3 {
		t.Fatalf("expected the safety tick to keep firing cheaply, got %d calls", cheap)
	}

	// Once the window elapses, exactly one pass escalates.
	for {
		select {
		case c := <-calls:
			if c.authoritative {
				return // the escalation happened
			}
		case <-time.After(3 * time.Second):
			t.Fatal("the authoritative pass never ran after the full-reconcile interval")
		}
	}
}

func TestRunIgnoresNonMarkdown(t *testing.T) {
	dir := t.TempDir()
	calls := make(chan struct{}, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, Options{
		Root:     dir,
		Debounce: 20 * time.Millisecond,
		OnReindex: func(authoritative bool) (Result, error) {
			calls <- struct{}{}
			return Result{}, nil
		},
	})

	waitCall(t, calls, "startup reconcile")

	// A non-markdown file must not trigger a reconcile.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-calls:
		t.Fatal("a .txt write should not trigger a reconcile")
	case <-time.After(300 * time.Millisecond):
		// expected: no reconcile
	}

	// But a markdown file in a newly created subdirectory should.
	sub := filepath.Join(dir, "decisions")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "d.md"), []byte("# D\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitCall(t, calls, "reconcile after note in new subdir")
}
