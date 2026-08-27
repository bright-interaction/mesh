// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestAwaitOwnerPreCanceledDoesNotStartProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var probed atomic.Bool
	var s Store
	err := s.awaitOwner(ctx, time.Second, func(context.Context) (bool, error) {
		probed.Store(true)
		return false, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("awaitOwner returned %v, want context.Canceled", err)
	}
	if probed.Load() {
		t.Fatal("pre-canceled owner wait started a readiness probe")
	}
}

func TestAwaitOwnerCancellationStopsInFlightProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	done := make(chan error, 1)
	var s Store
	go func() {
		done <- s.awaitOwner(ctx, time.Second, func(probeCtx context.Context) (bool, error) {
			close(entered)
			<-probeCtx.Done()
			return false, probeCtx.Err()
		})
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("owner wait did not enter its readiness probe")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("awaitOwner returned %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("owner wait did not cancel its in-flight readiness probe")
	}
}

func TestAwaitOwnerBoundCancelsInFlightProbe(t *testing.T) {
	entered := make(chan struct{})
	var s Store
	start := time.Now()
	err := s.awaitOwner(context.Background(), 25*time.Millisecond, func(probeCtx context.Context) (bool, error) {
		close(entered)
		<-probeCtx.Done()
		return false, probeCtx.Err()
	})
	if !errors.Is(err, ErrOwnerNotIndexing) {
		t.Fatalf("awaitOwner returned %v, want ErrOwnerNotIndexing", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("owner timeout did not bound an in-flight readiness probe: %s", elapsed)
	}
	select {
	case <-entered:
	default:
		t.Fatal("test did not exercise an in-flight readiness probe")
	}
}

func TestDriftReportContextCancellationDuringNoteRead(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := s.driftReportContext(ctx, root,
			func(context.Context, string) ([]string, error) {
				return []string{filepath.Join(root, "blocked.md")}, nil
			},
			func(parseCtx context.Context, _ string) (*ParsedNote, error) {
				close(entered)
				<-parseCtx.Done()
				return nil, parseCtx.Err()
			})
		done <- err
	}()

	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("drift report did not enter the note read")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("DriftReportContext returned %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("drift report did not cancel its in-flight note read")
	}
}

func TestReadOnlyOwnerWaitPreCanceledDoesNotScanVault(t *testing.T) {
	root := t.TempDir()
	owner, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadOnly(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// If the cancellation guard regresses, probing this deliberately missing root
	// returns a filesystem error instead of context.Canceled.
	err = reader.AwaitOwnerCaughtUp(ctx, filepath.Join(root, "missing"), time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("read-only AwaitOwnerCaughtUp returned %v, want context.Canceled", err)
	}
}

func TestOpQueuedContextPreCanceled(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.OpQueuedContext(ctx, "op.json"); !errors.Is(err, context.Canceled) {
		t.Fatalf("OpQueuedContext returned %v, want context.Canceled", err)
	}
}

func TestPendingDriftReportsFormerlyDroppedNoteAfterItIsFixed(t *testing.T) {
	root := t.TempDir()
	notePath := filepath.Join(root, "broken.md")
	broken := "---\nid: broken\ntype: note\nupdated: 2026-06-18 (post-mortem: root cause)\n---\n# Broken\n"
	if err := os.WriteFile(notePath, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}

	owner, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReindexFull(owner, root); err != nil {
		owner.Close()
		t.Fatal(err)
	}
	if dropped := mustDropped(t, owner); len(dropped) != 1 || dropped[0].Path != "broken.md" {
		owner.Close()
		t.Fatalf("initial pass did not record the broken note: %+v", dropped)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}

	fixed := "---\nid: broken\ntype: note\nwhen: 2026-01-01\n---\n# Fixed\n"
	if err := os.WriteFile(notePath, []byte(fixed), 0o644); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadOnly(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	drift, err := reader.PendingDrift(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(drift.Added) != 1 || drift.Added[0] != "broken.md" || len(drift.Changed) != 0 || len(drift.Removed) != 0 {
		t.Fatalf("fixed note must remain pending until the owner indexes it, got %+v", drift)
	}
}
