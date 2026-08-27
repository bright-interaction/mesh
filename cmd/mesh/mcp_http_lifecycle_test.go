// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"context"
	"testing"
	"time"
)

func TestHTTPBackgroundWatcherIsJoinedBeforeShutdownReturns(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})
	stop := startMCPBackgroundWatch(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
	})

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("background watcher did not start")
	}
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("stop did not cancel the watcher")
	}
	select {
	case <-stopped:
		t.Fatal("stop returned while the watcher was still using the MCP server")
	default:
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("stop did not join the watcher after it exited")
	}
}
