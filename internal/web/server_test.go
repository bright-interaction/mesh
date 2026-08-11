// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testServer(t *testing.T) *httptest.Server {
	ts, _ := testServerVault(t)
	return ts
}

// testServerVault is testServer plus the vault path, for the tests that need to play
// the owning writer as well as the viewer.
func testServerVault(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("hub.md", "---\nid: hub\ntype: note\nwhen: 2026-01-01\ntags: [core]\n---\n# Hub\n[[alpha]] [[beta]]\n")
	write("alpha.md", "---\nid: alpha\ntype: note\nwhen: 2026-01-01\n---\n# Alpha\n[[beta]]\n")
	write("beta.md", "---\nid: beta\ntype: note\nwhen: 2026-01-01\n---\n# Beta\nleaf\n")

	seedIndex(t, dir)
	s, err := NewServer(dir)
	if err != nil {
		t.Fatalf("NewServer (is git/index ok?): %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { ts.Close(); s.Close() })
	return ts, dir
}

func get(t *testing.T, ts *httptest.Server, path string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestServerRoutes(t *testing.T) {
	ts := testServer(t)

	// SPA shell.
	code, body, _ := get(t, ts, "/")
	if code != 200 || !strings.Contains(body, "<canvas") {
		t.Fatalf("/ should serve the SPA shell: %d", code)
	}

	// graph.json is valid + carries the notes.
	code, body, hdr := get(t, ts, "/graph.json")
	if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "application/json") {
		t.Fatalf("/graph.json: %d %s", code, hdr.Get("Content-Type"))
	}
	var exp Export
	if err := json.Unmarshal([]byte(body), &exp); err != nil {
		t.Fatalf("graph.json not valid Export: %v", err)
	}
	if exp.Meta.NodeCount != 3 || exp.Meta.IndexID == "" {
		t.Fatalf("graph.json should have 3 notes + an index, got %+v", exp.Meta)
	}

	// Assets serve with the right content types.
	for _, tc := range []struct{ path, ctype string }{
		{"/assets/app.js", "application/javascript"},
		{"/assets/style.css", "text/css"},
		{"/assets/fonts/geist.woff2", "font/woff2"},
	} {
		code, _, hdr := get(t, ts, tc.path)
		if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), tc.ctype) {
			t.Fatalf("%s: %d %s (want %s)", tc.path, code, hdr.Get("Content-Type"), tc.ctype)
		}
	}

	// Traversal + unknown assets are refused.
	if code, _, _ := get(t, ts, "/assets/../server.go"); code == 200 {
		t.Fatal("path traversal must not serve files outside assets")
	}
	if code, _, _ := get(t, ts, "/assets/nope.js"); code != 404 {
		t.Fatalf("unknown asset should 404, got %d", code)
	}
}

func TestDashboardAPI(t *testing.T) {
	ts, dir := testServerVault(t)
	// The viewer is read-only, so its usage counters reach the index through the owning
	// writer's op queue (see index.OpTelemetry). Without an owner they would sit in the
	// queue, which is exactly what the dashboard would then be reporting: nothing.
	runOwner(t, dir)
	// A search bumps the queries counter the dashboard reports.
	if code, _, _ := get(t, ts, "/api/search?q=alpha"); code != 200 {
		t.Fatalf("search status %d", code)
	}
	var code int
	var body string
	// The queue is drained by the owner's reconcile, so the counter lands within a tick
	// rather than synchronously with the search.
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, body, _ = get(t, ts, "/api/dashboard")
		if code != 200 {
			t.Fatalf("dashboard status %d", code)
		}
		if strings.Contains(body, `"queries":0`) && time.Now().Before(deadline) {
			time.Sleep(25 * time.Millisecond)
			continue
		}
		break
	}
	var d struct {
		Usage struct {
			Queries int `json:"queries"`
			Notes   int `json:"notes"`
		} `json:"usage"`
		EstTokensSaved int            `json:"est_tokens_saved"`
		Coverage       map[string]int `json:"coverage"`
	}
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatal(err)
	}
	if d.Usage.Queries < 1 {
		t.Errorf("queries counter = %d, want >= 1", d.Usage.Queries)
	}
	if d.Usage.Notes != 3 {
		t.Errorf("notes = %d, want 3", d.Usage.Notes)
	}
	if d.EstTokensSaved < 1 {
		t.Errorf("est_tokens_saved = %d, want > 0 after a query", d.EstTokensSaved)
	}
	if d.Coverage["note"] != 3 {
		t.Errorf("coverage[note] = %d, want 3", d.Coverage["note"])
	}
}

// An unrestricted caller (owner/admin, or the standalone viewer) must get a real
// counts.notes. The web app decides whether to show the "this vault is empty"
// overlay from that number, so a zero here hid a fully populated vault behind an
// empty-state card for exactly the people allowed to see all of it. The scoped
// branch counted notes in its own loop; the unrestricted branch never assigned the
// variable at all.
func TestStatusCountsNotesUnrestricted(t *testing.T) {
	ts := testServer(t)

	code, body, _ := get(t, ts, "/api/status")
	if code != 200 {
		t.Fatalf("/api/status = %d, want 200", code)
	}
	var st struct {
		Counts map[string]int `json:"counts"`
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if st.Counts["notes"] != 3 {
		t.Errorf("counts.notes = %d, want 3 (the vault has 3 notes; 0 trips the empty-state overlay)", st.Counts["notes"])
	}
	if st.Counts["nodes"] < 3 {
		t.Errorf("counts.nodes = %d, want >= 3", st.Counts["nodes"])
	}
}

// handleStatus read s.graph with no lock at all while handleGraph took s.mu.RLock for
// the same field and refresh / reindex / pending-promote took s.mu.Lock to swap it. Only
// the scope- or folder-confined branch of handleStatus walks the graph, so a scope
// resolver is what makes the read reachable. The SPA polls /api/status on a timer to
// drive its empty-state overlay, so this raced a promote or a reindex in ordinary use.
// Fails under -race before the fix.
func TestStatusGraphReadTakesTheLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dev.md"),
		[]byte("---\nid: dev\ntype: note\nwhen: 2026-01-01\nscope: dev\n---\n# Dev\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedIndex(t, dir)
	s, err := NewServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Registered before the stopper below so it runs after it (cleanups are LIFO):
	// closing the store under a live refresh loop is not what this test measures.
	t.Cleanup(func() { s.Close() })
	s.SetScopeResolver(func(*http.Request) map[string]bool { return map[string]bool{"dev": true} })
	h := s.Handler()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := s.refresh(); err != nil {
				t.Errorf("refresh: %v", err)
				return
			}
		}
	}()
	t.Cleanup(func() { close(stop); wg.Wait() })

	for i := 0; i < 200; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
		if rec.Code != 200 {
			t.Fatalf("/api/status = %d, want 200", rec.Code)
		}
	}
}
