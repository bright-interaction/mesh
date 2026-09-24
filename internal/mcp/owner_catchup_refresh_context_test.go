// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestOwnerCatchUpCancellationBeforeReaderInstall(t *testing.T) {
	root := t.TempDir()
	seedIndex(t, root)
	srv, err := NewServer(root)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if err := srv.WaitReady(); err != nil {
		t.Fatal(err)
	}
	oldGraph, oldRetriever := srv.snapshot()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.viewReusable = false // exercise actual construction, not unchanged-view reuse
	srv.beforeReaderInstall = cancel
	_, err = srv.awaitOwnerCaughtUp(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("refresh lost caller cancellation: %v", err)
	}
	g, r := srv.snapshot()
	if g != oldGraph || r != oldRetriever {
		t.Fatal("canceled catch-up published a reader snapshot")
	}
	// Cancellation must not poison an independent later request.
	srv.beforeReaderInstall = nil
	if _, err := srv.awaitOwnerCaughtUp(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerCatchUpRefreshLockHonorsCallerDeadline(t *testing.T) {
	root := t.TempDir()
	seedIndex(t, root)
	srv, err := NewServer(root)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if err := srv.WaitReady(); err != nil {
		t.Fatal(err)
	}
	oldGraph, oldRetriever := srv.snapshot()
	srv.reloadMu.Lock()
	locked, joined := true, false
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := srv.awaitOwnerCaughtUp(ctx); done <- err }()
	defer func() {
		if locked {
			srv.reloadMu.Unlock()
		}
		if !joined {
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("catch-up worker failed to join after releasing test lock")
			}
		}
	}()
	select {
	case err := <-done:
		joined = true
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v, want caller deadline", err)
		}
	case <-time.After(time.Second):
		t.Fatal("reader catch-up refresh ignored caller deadline while waiting for reload lock")
	}
	g, r := srv.snapshot()
	if g != oldGraph || r != oldRetriever {
		t.Fatal("deadline changed published snapshot")
	}
	srv.reloadMu.Unlock()
	locked = false
}
