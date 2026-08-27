// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/retrieve"
)

func TestRetrieverContextCancellationDoesNotPoisonGateOrCache(t *testing.T) {
	s, _ := cfgServer(t)
	s.cachedRetriever.Store(nil)
	started := make(chan struct{})
	s.buildRetriever = func(ctx context.Context, _ *index.Store, _ *graph.Graph) (*retrieve.Retriever, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := s.retrieverContext(ctx)
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("retrieverContext error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("active retriever build did not stop after cancellation")
	}
	if s.cachedRetriever.Load() != nil {
		t.Fatal("canceled retriever build was cached")
	}
	s.buildRetriever = retrieve.NewFromEnvContext
	if rt, err := s.retrieverContext(context.Background()); err != nil || rt == nil {
		t.Fatalf("gate was poisoned after cancellation: retriever = %v, err = %v", rt, err)
	}
}

func TestRetrieverContextWaiterCanCancel(t *testing.T) {
	s, _ := cfgServer(t)
	s.cachedRetriever.Store(nil)
	started, release := make(chan struct{}), make(chan struct{})
	s.buildRetriever = func(ctx context.Context, st *index.Store, g *graph.Graph) (*retrieve.Retriever, error) {
		close(started)
		<-release
		return retrieve.NewContext(ctx, st, g)
	}
	first := make(chan error, 1)
	go func() {
		_, err := s.retrieverContext(context.Background())
		first <- err
	}()
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	waiter := make(chan error, 1)
	go func() {
		_, err := s.retrieverContext(ctx)
		waiter <- err
	}()
	cancel()
	select {
	case err := <-waiter:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting retriever error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("retriever waiter remained blocked behind the active build")
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first build failed: %v", err)
	}
}

func TestRetrieverConfigInvalidationWinsAgainstInflightBuild(t *testing.T) {
	s, _ := cfgServer(t)
	s.cachedRetriever.Store(nil)
	started, release := make(chan struct{}), make(chan struct{})
	var builds atomic.Int32
	s.buildRetriever = func(ctx context.Context, st *index.Store, g *graph.Graph) (*retrieve.Retriever, error) {
		builds.Add(1)
		if builds.Load() == 1 {
			close(started)
			<-release
		}
		return retrieve.NewContext(ctx, st, g)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.retrieverContext(context.Background())
		done <- err
	}()
	<-started
	s.invalidateRetriever()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s.cachedRetriever.Load() == nil {
		t.Fatal("the in-flight call returned without rebuilding the invalidated config")
	}
	if _, err := s.retrieverContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if builds.Load() != 2 {
		t.Fatalf("build count = %d, want stale build plus one current rebuild", builds.Load())
	}
}
