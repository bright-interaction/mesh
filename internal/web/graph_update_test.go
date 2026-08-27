// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/index"
)

func owningServerForGraphUpdateTest(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "seed.md"), []byte("---\nid: seed\ntype: note\nwhen: 2026-01-01\n---\n# Seed\nx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := NewOwningServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close server: %v", err)
		}
	})
	return s
}

func TestGraphUpdatesSerializeRebuildThroughPublication(t *testing.T) {
	s := owningServerForGraphUpdateTest(t)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})
	calls := 0 // reindexStore is invoked only while graphUpdateGate is held
	s.reindexStore = func(ctx context.Context, _ *index.Store, _ string) (*graph.Graph, error) {
		calls++
		g := graph.New()
		if calls == 1 {
			g.AddNode(&graph.Node{ID: "note:first", Kind: "note", NoteID: "first"})
			close(firstEntered)
			select {
			case <-releaseFirst:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		} else {
			g.AddNode(&graph.Node{ID: "note:second", Kind: "note", NoteID: "second"})
			close(secondEntered)
		}
		return g, nil
	}

	errCh := make(chan error, 2)
	go func() { errCh <- s.reindexAndPublish(context.Background(), false) }()
	select {
	case <-firstEntered:
	case <-time.After(time.Second):
		t.Fatal("first rebuild did not start")
	}
	go func() { errCh <- s.reindexAndPublish(context.Background(), false) }()
	select {
	case <-secondEntered:
		t.Fatal("second rebuild entered while the first had not published")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	for range 2 {
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("reindex and publish: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("serialized rebuild did not finish")
		}
	}
	s.mu.RLock()
	_, finalIsSecond := s.graph.Node("note:second")
	s.mu.RUnlock()
	if !finalIsSecond {
		t.Fatal("the last serialized rebuild was not the final published graph")
	}
}

func TestGraphUpdateWaitHonorsCancellation(t *testing.T) {
	s := owningServerForGraphUpdateTest(t)
	release, err := s.acquireGraphUpdate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := s.acquireGraphUpdate(ctx)
		errCh <- err
	}()
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting graph update cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiting graph update ignored cancellation")
	}
	release()
}
