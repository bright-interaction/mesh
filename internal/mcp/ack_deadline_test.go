// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAcknowledgementDeadlineIncludesRefreshLock(t *testing.T) {
	for _, callerCanceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "owner deadline", true: "caller cancellation"}[callerCanceled], func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "published.md")
			if err := os.WriteFile(path, []byte("---\nid: published\ntype: note\n---\nExpected bytes.\n"), 0600); err != nil {
				t.Fatal(err)
			}
			seedIndex(t, root)
			srv, err := NewServer(root)
			if err != nil {
				t.Fatal(err)
			}
			defer srv.Close()
			if err := srv.WaitReady(); err != nil {
				t.Fatal(err)
			}
			srv.ownerIndexTimeout = 40 * time.Millisecond
			oldGraph, oldRetriever := srv.snapshot()
			srv.reloadMu.Lock()
			locked := true
			defer func() {
				if locked {
					srv.reloadMu.Unlock()
				}
			}()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if callerCanceled {
				cancel()
			}
			done := make(chan error, 1)
			go func() { done <- srv.awaitOwnerIndexed(ctx, "published", path) }()
			select {
			case err := <-done:
				want := ErrOwnerNotIndexing
				if callerCanceled {
					want = context.Canceled
				}
				if !errors.Is(err, want) {
					t.Fatalf("got %v, want %v", err, want)
				}
			case <-time.After(time.Second):
				t.Fatal("acknowledgement waited for refresh lock beyond its deadline")
			}
			g, r := srv.snapshot()
			if g != oldGraph || r != oldRetriever {
				t.Fatal("canceled refresh published a new snapshot")
			}
			srv.reloadMu.Unlock()
			locked = false
			srv.ownerIndexTimeout = time.Second
			if err := srv.awaitOwnerIndexed(context.Background(), "published", path); err != nil {
				t.Fatalf("later acknowledgement failed: %v", err)
			}
		})
	}
}

func TestWriteReceiptSurvivesBusyRefresh(t *testing.T) {
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
	srv.ownerIndexTimeout = 40 * time.Millisecond
	srv.reloadMu.Lock()
	defer srv.reloadMu.Unlock()
	// A real tool call must return the durable file's identity despite not acquiring
	// the refresh lock. A bare transport timeout loses this recovery information.
	out := writeNoteVia(t, srv, "busy refresh receipt")
	if out["index_stale"] != true || out["owner_down"] != true {
		t.Fatalf("missing honest stale receipt: %v", out)
	}
	if out["id"] != "busy-refresh-receipt" {
		t.Fatalf("missing durable note identity: %v", out)
	}
	if _, err := os.Stat(filepath.Join(root, "gotchas", "busy-refresh-receipt.md")); err != nil {
		t.Fatal(err)
	}
}
