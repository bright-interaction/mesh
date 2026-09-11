//go:build darwin || linux

// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"
)

func TestAcknowledgementDeadlineIncludesRetrieverConfigRead(t *testing.T) {
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
	oldGraph, oldRetriever := srv.snapshot()
	oldHashes := srv.viewHashes
	config := filepath.Join(root, ".mesh", "config.toml")
	if err := syscall.Mkfifo(config, 0600); err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Release a read-only worker parked in FIFO open after its caller canceled.
		if fd, err := syscall.Open(config, syscall.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			syscall.Close(fd)
		}
		os.Remove(config)
	}()
	srv.ownerIndexTimeout = 100 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- srv.awaitOwnerIndexed(context.Background(), "published", path) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrOwnerNotIndexing) {
			t.Fatalf("got %v, want stale receipt", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retriever construction escaped acknowledgement deadline")
	}
	g, r := srv.snapshot()
	if g != oldGraph || r != oldRetriever || !reflect.DeepEqual(srv.viewHashes, oldHashes) {
		t.Fatal("canceled construction changed published view")
	}
}
