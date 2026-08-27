// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadBytesContextCancellationDoesNotWaitForStalledRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		data, err := readBytesContext(ctx, func() ([]byte, error) {
			close(started)
			<-release
			close(finished)
			return []byte("late result"), nil
		})
		if data != nil {
			done <- errors.New("cancelled read exposed late bytes")
			return
		}
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("read returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled read waited for the stalled filesystem operation")
	}

	// The isolated reader may finish later. Its private buffered publication must let
	// it exit even though the cancelled caller is no longer receiving.
	close(release)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("late reader did not exit")
	}
}

func TestReadBytesContextAlreadyCancelledDoesNotStartRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	data, err := readBytesContext(ctx, func() ([]byte, error) {
		called = true
		return []byte("unexpected"), nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("read returned %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("already-cancelled read invoked the filesystem operation")
	}
	if data != nil {
		t.Fatalf("already-cancelled read returned %q", data)
	}
}

type cancelOnRead struct {
	cancel context.CancelFunc
	calls  int
}

func (r *cancelOnRead) Read(p []byte) (int, error) {
	r.calls++
	copy(p, "bytes that must not be published")
	r.cancel()
	return len("bytes that must not be published"), nil
}

func TestReadAllCooperativelyStopsBeforePublishingCancelledChunk(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &cancelOnRead{cancel: cancel}
	data, err := readAllCooperatively(ctx, reader)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cooperative read returned %v, want context.Canceled", err)
	}
	if data != nil {
		t.Fatalf("cooperative read published cancelled chunk %q", data)
	}
	if reader.calls != 1 {
		t.Fatalf("cooperative read made %d reads after cancellation, want 1 total", reader.calls)
	}
}

func TestFileInfoContextCancellationDoesNotWaitForStalledStat(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		info, err := fileInfoContext(ctx, func() (os.FileInfo, error) {
			close(started)
			<-release
			return nil, errors.New("late stat")
		})
		if info != nil {
			done <- errors.New("cancelled stat exposed late file info")
			return
		}
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stat returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled stat waited for the stalled filesystem operation")
	}
	close(release)
}

func TestReadFileHeadContextBoundsAndAcceptsShortRead(t *testing.T) {
	dir := t.TempDir()
	short := filepath.Join(dir, "short.md")
	if err := os.WriteFile(short, []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFileHeadContext(context.Background(), short, 64)
	if err != nil {
		t.Fatalf("short head read: %v", err)
	}
	if string(got) != "short" {
		t.Fatalf("short head = %q, want %q", got, "short")
	}

	long := filepath.Join(dir, "long.md")
	if err := os.WriteFile(long, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = ReadFileHeadContext(context.Background(), long, 4)
	if err != nil {
		t.Fatalf("bounded head read: %v", err)
	}
	if string(got) != "0123" {
		t.Fatalf("bounded head = %q, want %q", got, "0123")
	}
}
