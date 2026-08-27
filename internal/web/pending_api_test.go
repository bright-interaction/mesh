// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
)

func TestPendingPromoteFinishesIndexingAfterClientCancellation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "seed.md"), []byte("---\nid: seed\ntype: note\nwhen: 2026-01-01\n---\n# Seed\nx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lifetime, stop := context.WithCancel(context.Background())
	defer stop()
	s, err := NewOwningServerContext(lifetime, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := index.PendingNote{Type: "gotcha", Title: "Finish after disconnect", Do: "keep indexing"}
	if err := s.store.AddPending(p); err != nil {
		t.Fatal(err)
	}
	id := index.PendingID(p.Type, p.Title)

	requestCtx, disconnect := context.WithCancel(context.Background())
	disconnect()
	req := httptest.NewRequest(http.MethodPost, "/api/pending/promote",
		strings.NewReader(`{"id":"`+id+`"}`)).WithContext(requestCtx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("promote after client cancellation = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.ID == "" {
		t.Fatalf("promote response: id=%q err=%v body=%s", out.ID, err, rec.Body.String())
	}
	if _, err := s.store.NotePath(out.ID); err != nil {
		t.Fatalf("durable note was not indexed after disconnect: %v", err)
	}
	s.mu.RLock()
	_, inLiveGraph := s.graph.Node("note:" + out.ID)
	s.mu.RUnlock()
	if !inLiveGraph {
		t.Fatal("durable note reached SQLite but not the owning UI's live graph")
	}
	if _, err := s.store.GetPending(id); err == nil {
		t.Fatal("promoted candidate remained in the queue after disconnect")
	}
}

func TestPendingPromoteQueuesCleanupWhenShutdownFollowsFileCreation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "seed.md"), []byte("---\nid: seed\ntype: note\nwhen: 2026-01-01\n---\n# Seed\nx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lifetime, stop := context.WithCancel(context.Background())
	s, err := NewOwningServerContext(lifetime, dir)
	if err != nil {
		t.Fatal(err)
	}
	p := index.PendingNote{Type: "gotcha", Title: "Clean up after shutdown", Do: "do not duplicate"}
	if err := s.store.AddPending(p); err != nil {
		t.Fatal(err)
	}
	pendingID := index.PendingID(p.Type, p.Title)
	// Cancel at the exact durable boundary: the note file exists, but none of its
	// SQLite bookkeeping or the owning reindex has begun. The replacement must drain
	// the queued compensation and index that published file.
	s.afterPendingFilePublished = stop
	req := httptest.NewRequest(http.MethodPost, "/api/pending/promote",
		strings.NewReader(`{"id":"`+pendingID+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("promote during shutdown = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.ID == "" {
		t.Fatalf("promote response: id=%q err=%v body=%s", out.ID, err, rec.Body.String())
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close old owner: %v", err)
	}

	replacement, err := NewOwningServer(dir)
	if err != nil {
		t.Fatalf("start replacement owner: %v", err)
	}
	defer replacement.Close()
	if _, err := replacement.store.GetPending(pendingID); err == nil {
		t.Fatal("replacement left the promoted candidate queued for duplicate promotion")
	}
	if _, err := replacement.store.NotePath(out.ID); err != nil {
		t.Fatalf("replacement did not index the note published during shutdown: %v", err)
	}
}

func postJSON(t *testing.T, ts *httptest.Server, path, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(ts.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b := make([]byte, 1<<16)
	n, _ := resp.Body.Read(b)
	return resp.StatusCode, string(b[:n])
}

// The review queue round-trip: a pending candidate lists, promotes into a real note
// (and leaves the queue), and another discards (leaves the queue, no note).
func TestPendingPromoteAndDiscard(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "seed.md"), []byte("---\nid: seed\ntype: note\nwhen: 2026-01-01\n---\n# Seed\nx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedIndex(t, dir)
	// The queue is filled by the extractor (a writer) and resolved through the owning
	// writer, so this exercises the production shape: a read-only viewer in front of a
	// live owner.
	seedPending(t, dir,
		index.PendingNote{Type: "gotcha", Title: "Keep me", Do: "do x", Dont: "dont y", Why: "because"},
		index.PendingNote{Type: "decision", Title: "Toss me"},
	)
	runOwner(t, dir)
	s, err := NewServer(dir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer s.Close()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	st, body, _ := get(t, ts, "/api/pending")
	if st != 200 || !strings.Contains(body, "Keep me") || !strings.Contains(body, "Toss me") {
		t.Fatalf("list = %d %s", st, body)
	}

	keepID := index.PendingID("gotcha", "Keep me")
	tossID := index.PendingID("decision", "Toss me")

	if st, body := postJSON(t, ts, "/api/pending/promote", `{"id":"`+keepID+`"}`); st != 200 || !strings.Contains(body, "promoted") {
		t.Fatalf("promote = %d %s", st, body)
	}
	if _, err := s.store.GetPending(keepID); err == nil {
		t.Fatal("promoted note is still in the pending queue")
	}
	// The promoted candidate is now a real gotcha note in the vault.
	if _, err := os.Stat(filepath.Join(dir, "gotchas")); err != nil {
		t.Fatalf("promoted note dir not created: %v", err)
	}
	// Promoting must stamp the note in the flywheel (authored count), like a direct
	// mesh_append_note. Startup backfill saw only the non-agent seed note (authored 0),
	// so an authored count here proves the promote-time RecordWriteback fired.
	if fw, err := s.store.FlywheelStats(); err != nil || fw.Authored < 1 {
		t.Errorf("promoted candidate should count as an authored write-back, authored=%d err=%v", fw.Authored, err)
	}

	if st, _ := postJSON(t, ts, "/api/pending/discard", `{"id":"`+tossID+`"}`); st != 200 {
		t.Fatalf("discard = %d", st)
	}
	st, body, _ = get(t, ts, "/api/pending")
	if strings.Contains(body, "Keep me") || strings.Contains(body, "Toss me") {
		t.Fatalf("queue should be empty: %s", body)
	}

	// Unknown id is a clean 404, not a 500.
	if st, _ := postJSON(t, ts, "/api/pending/promote", `{"id":"nope"}`); st != 404 {
		t.Fatalf("promote unknown = %d, want 404", st)
	}
}
