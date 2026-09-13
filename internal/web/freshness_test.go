// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/retrieve"
)

func freshRequest(s *Server, route string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, route, nil))
	return w
}

func TestViewerStartupLoadsOnceAndFirstReadReusesGraph(t *testing.T) {
	dir := t.TempDir()
	seedIndex(t, dir)
	loads := 0
	s, err := newReadOnlyServerContext(context.Background(), dir, func(ctx context.Context, s *Server) (*graph.Graph, error) {
		loads++
		return s.store.LoadGraphContext(ctx)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.updateCheck = nil
	startupGraph := s.graph
	if loads != 1 || startupGraph == nil || !s.viewReusable || s.changeMonitor == nil {
		t.Fatalf("startup did not publish one stamped graph: loads=%d reusable=%v", loads, s.viewReusable)
	}
	for _, route := range []string{"/api/status", "/graph.json", "/api/status"} {
		if w := freshRequest(s, route); w.Code != 200 {
			t.Fatalf("%s: %d %s", route, w.Code, w.Body.String())
		}
	}
	if loads != 1 || s.graph != startupGraph {
		t.Fatalf("unchanged first reads repeated startup load: loads=%d", loads)
	}
}

func TestViewerStartupBracketsExternalCommit(t *testing.T) {
	for _, continuous := range []bool{false, true} {
		t.Run(fmt.Sprintf("continuous-writes=%v", continuous), func(t *testing.T) {
			dir := t.TempDir()
			seedIndex(t, dir)
			loads := 0
			var opened *Server
			s, err := newReadOnlyServerContext(context.Background(), dir, func(ctx context.Context, current *Server) (*graph.Graph, error) {
				opened = current
				g, err := current.store.LoadGraphContext(ctx)
				if err != nil {
					return nil, err
				}
				loads++
				if loads == 1 || continuous {
					writeNote(t, dir, "startup-race.md", fmt.Sprintf("---\nid: startup-race\ntype: note\nwhen: 2026-01-01\n---\n# Startup race\nrevision %d\n", loads))
					seedIndex(t, dir)
				}
				return g, nil
			})
			if continuous {
				if err == nil || s != nil || opened == nil || !opened.viewClosed || opened.viewReusable || opened.graph != nil || loads != 2 {
					t.Fatalf("unstable startup published graph or leaked server: err=%v loads=%d", err, loads)
				}
				assertStartupReaderClosed(t, opened)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			if _, found := s.graph.Node("note:startup-race"); loads != 2 || !found || !s.viewReusable {
				t.Fatalf("startup blessed earlier graph: loads=%d found=%v reusable=%v", loads, found, s.viewReusable)
			}
			if w := freshRequest(s, "/graph.json"); w.Code != 200 || loads != 2 || !strings.Contains(w.Body.String(), "startup-race") {
				t.Fatalf("stable startup was not reused: status=%d loads=%d", w.Code, loads)
			}
		})
	}
}

func assertStartupReaderClosed(t *testing.T, s *Server) {
	t.Helper()
	if s.changeMonitor == nil {
		t.Fatal("test did not exercise an opened monitor")
	}
	if _, err := s.changeMonitor.ReaderVersion(context.Background()); !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("startup monitor was not closed: %v", err)
	}
	if _, err := s.store.LoadGraphContext(context.Background()); err == nil || !strings.Contains(err.Error(), "database is closed") {
		t.Fatalf("startup read store was not closed: %v", err)
	}
}

func TestViewerCanceledStartupClosesMonitorAndStore(t *testing.T) {
	for _, returnGraph := range []bool{false, true} {
		t.Run(fmt.Sprintf("loader-returns-graph=%v", returnGraph), func(t *testing.T) {
			dir := t.TempDir()
			seedIndex(t, dir)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var opened *Server
			s, err := newReadOnlyServerContext(ctx, dir, func(ctx context.Context, current *Server) (*graph.Graph, error) {
				opened = current
				g, err := current.store.LoadGraphContext(ctx)
				if err != nil {
					return nil, err
				}
				cancel()
				if returnGraph {
					return g, nil // cancellation must be checked even if a loader returns success
				}
				return nil, ctx.Err()
			})
			if !errors.Is(err, context.Canceled) || s != nil || opened == nil || !opened.viewClosed || opened.viewReusable || opened.graph != nil {
				t.Fatalf("canceled startup published graph or leaked server: %v", err)
			}
			assertStartupReaderClosed(t, opened)
		})
	}
}

func TestViewerStartupFailsClosedForReplacedIndex(t *testing.T) {
	dir := t.TempDir()
	seedIndex(t, dir)
	db := filepath.Join(dir, ".mesh", "mesh.db")
	backup := db + ".test-backup"
	var opened *Server
	s, err := newReadOnlyServerContext(context.Background(), dir, func(ctx context.Context, current *Server) (*graph.Graph, error) {
		opened = current
		g, err := current.store.LoadGraphContext(ctx)
		if err != nil {
			return nil, err
		}
		if err := os.Rename(db, backup); err != nil {
			return nil, err
		}
		t.Cleanup(func() { _ = os.Rename(backup, db) })
		if err := os.WriteFile(db, []byte("replacement"), 0600); err != nil {
			return nil, err
		}
		return g, nil
	})
	if !errors.Is(err, index.ErrMonitorIndexReplaced) || s != nil || opened == nil || !opened.viewClosed || opened.viewReusable || opened.graph != nil {
		t.Fatalf("startup accepted replacement index: %v", err)
	}
	// Restore the original identity so the monitor probe checks connection
	// closure, not its earlier identity guard.
	if err := os.Rename(backup, db); err != nil {
		t.Fatal(err)
	}
	assertStartupReaderClosed(t, opened)
}

func TestViewerReadsObserveExternalOwnerCommits(t *testing.T) {
	s, dir := cfgServer(t)
	s.updateCheck = nil
	s.buildRetriever = func(ctx context.Context, st *index.Store, g *graph.Graph) (*retrieve.Retriever, error) {
		return retrieve.NewContext(ctx, st, g) // no configured model/embedding endpoint
	}
	for _, route := range []string{"/graph.json", "/api/search?q=body"} {
		if w := freshRequest(s, route); w.Code != 200 {
			t.Fatalf("initial %s: %d %s", route, w.Code, w.Body.String())
		}
	}
	oldGraph, oldRetriever := s.graph, s.cachedRetriever.Load()
	if oldRetriever == nil {
		t.Fatal("search did not warm retriever")
	}
	writeNote(t, dir, "external.md", "---\nid: external\ntype: note\nwhen: 2026-01-01\n---\n# External\nunicornzeppelin\n")
	seedIndex(t, dir) // a separate writer, no viewer write endpoint
	for _, route := range []string{"/graph.json", "/api/search?q=unicornzeppelin", "/api/note/external"} {
		w := freshRequest(s, route)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "external") {
			t.Fatalf("external commit missing from %s: %d %s", route, w.Code, w.Body.String())
		}
	}
	if s.graph == oldGraph || s.cachedRetriever.Load() == oldRetriever {
		t.Fatal("external commit reused stale graph/retriever")
	}
	if !s.store.ReadOnly() || s.owner != nil {
		t.Fatal("viewer acquired writer ownership")
	}
	if _, live := index.OwnerStatus(filepath.Join(dir, ".mesh")); live {
		t.Fatal("viewer left a second owner")
	}
	if err := os.Remove(filepath.Join(dir, "external.md")); err != nil {
		t.Fatal(err)
	}
	seedIndex(t, dir)
	for _, route := range []string{"/graph.json", "/api/search?q=unicornzeppelin"} {
		w := freshRequest(s, route)
		if w.Code != 200 || strings.Contains(w.Body.String(), `"external"`) {
			t.Fatalf("deleted note retained by %s: %d %s", route, w.Code, w.Body.String())
		}
	}
}

func TestViewerFreshnessSharesReloadAndReusesUnchangedGraph(t *testing.T) {
	s, _ := cfgServer(t)
	s.viewReusable = false // exercise one shared refresh after the stamped startup load
	loads := 0             // graph gate serializes all callers
	s.loadFreshGraph = func(ctx context.Context) (*graph.Graph, error) { loads++; return s.store.LoadGraphContext(ctx) }
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.ensureFresh(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if loads != 1 {
		t.Fatalf("unchanged concurrent reads loaded graph %d times", loads)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.ensureFresh(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read: %v", err)
	}
}

func TestViewerFreshnessNeverLabelsEarlierGraphWithLaterRevision(t *testing.T) {
	s, dir := cfgServer(t)
	s.viewReusable = false
	loads := 0
	s.loadFreshGraph = func(ctx context.Context) (*graph.Graph, error) {
		g, err := s.store.LoadGraphContext(ctx)
		if err != nil {
			return nil, err
		}
		loads++
		writeNote(t, dir, "racing.md", fmt.Sprintf("---\nid: racing\ntype: note\nwhen: 2026-01-01\n---\n# Race\nrevision %d\n", loads))
		seedIndex(t, dir)
		return g, nil
	}
	oldGraph := s.graph
	w := freshRequest(s, "/graph.json")
	if w.Code != 503 || s.viewReusable || s.graph != oldGraph || loads != 2 {
		t.Fatalf("racing load was published/reused: status=%d loads=%d reusable=%v", w.Code, loads, s.viewReusable)
	}
	s.loadFreshGraph = nil
	if w := freshRequest(s, "/graph.json"); w.Code != 200 || !strings.Contains(w.Body.String(), "racing") {
		t.Fatalf("stable retry did not recover: %d %s", w.Code, w.Body.String())
	}
}

func TestViewerFreshnessFailsClosedForReplacedIndex(t *testing.T) {
	for _, recreate := range []bool{false, true} {
		t.Run(fmt.Sprintf("monitor-recreated=%v", recreate), func(t *testing.T) {
			s, dir := cfgServer(t)
			if recreate {
				if err := s.changeMonitor.Close(); err != nil {
					t.Fatal(err)
				}
				s.changeMonitor = nil
			}
			db := filepath.Join(dir, ".mesh", "mesh.db")
			backup := db + ".test-backup"
			if err := os.Rename(db, backup); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Rename(backup, db) })
			if err := os.WriteFile(db, []byte("replacement"), 0600); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				w := freshRequest(s, "/graph.json")
				if w.Code != 503 || strings.Contains(w.Body.String(), dir) || s.viewReusable {
					t.Fatalf("replaced index not safely refused: %d %s", w.Code, w.Body.String())
				}
			}
		})
	}
}

func TestViewerVectorOnlyCommitInvalidatesRetriever(t *testing.T) {
	s, dir := cfgServer(t)
	if err := s.ensureFresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.buildRetriever = func(ctx context.Context, st *index.Store, g *graph.Graph) (*retrieve.Retriever, error) {
		return retrieve.NewContext(ctx, st, g)
	}
	if _, err := s.retrieverContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.cachedRetriever.Load() == nil {
		t.Fatal("retriever was not warmed")
	}
	writer, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := writer.ReplaceVectors("test", []index.VectorRow{{NodeID: "note:n", Vec: []float32{1, 0}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureFresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.cachedRetriever.Load() != nil {
		t.Fatal("vector-only external commit reused cached vectors")
	}
}

func TestViewerRefreshCancellationReleasesGateWithoutPublishing(t *testing.T) {
	s, _ := cfgServer(t)
	s.viewReusable = false
	started := make(chan struct{})
	s.loadFreshGraph = func(ctx context.Context) (*graph.Graph, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	oldGraph := s.graph
	go func() { done <- s.ensureFresh(ctx) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("refresh did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled refresh: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled refresh did not return")
	}
	if s.viewReusable || s.graph != oldGraph {
		t.Fatal("canceled refresh was published")
	}
	s.loadFreshGraph = nil
	if err := s.ensureFresh(context.Background()); err != nil {
		t.Fatalf("retry after cancellation: %v", err)
	}
}

func TestViewerReadinessIsAuthenticatedAndTruthful(t *testing.T) {
	s, dir := cfgServer(t)
	s.updateCheck = nil
	s.auth.token = "test-only-token"
	loads := 0
	s.viewReusable = false
	s.loadFreshGraph = func(ctx context.Context) (*graph.Graph, error) {
		loads++
		return s.store.LoadGraphContext(ctx)
	}
	if w := freshRequest(s, "/api/status"); w.Code != 401 || loads != 0 || s.viewReusable {
		t.Fatal("unauthorized request probed the index")
	}
	s.auth.token = ""
	status := func() map[string]any {
		t.Helper()
		w := freshRequest(s, "/api/status")
		var body struct {
			Viewer map[string]any `json:"viewer"`
		}
		if w.Code != 200 {
			t.Fatalf("status: %d %s", w.Code, w.Body.String())
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Viewer
	}
	view := status()
	if view["name"] != "mesh" || view["apiVersion"] != float64(1) || view["mode"] != "read-only" || view["freshness"] != "current-to-observed-index" || view["indexOwner"] != "not-observed" {
		t.Fatalf("readiness: %+v", view)
	}
	owner, err := index.AcquireOwnerLock(filepath.Join(dir, ".mesh"), "test owner", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Release() })
	if view := status(); view["indexOwner"] != "observed" {
		t.Fatalf("owner not observed: %+v", view)
	}
	if err := owner.Release(); err != nil {
		t.Fatal(err)
	}
	if view := status(); view["indexOwner"] != "not-observed" {
		t.Fatalf("departed owner still reported: %+v", view)
	}
	s.member = &memberAuth{}
	view = s.viewerReadiness(httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if view["indexOwner"] != "unknown" || view["freshness"] != "unknown" {
		t.Fatalf("team or unobserved status overclaims: %+v", view)
	}
}
