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
			OnReindex: func() (Result, error) {
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
		OnReindex: func() (Result, error) {
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

func TestRunIgnoresNonMarkdown(t *testing.T) {
	dir := t.TempDir()
	calls := make(chan struct{}, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Run(ctx, Options{
		Root:     dir,
		Debounce: 20 * time.Millisecond,
		OnReindex: func() (Result, error) {
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
