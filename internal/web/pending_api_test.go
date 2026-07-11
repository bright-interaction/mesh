// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/index"
)

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
	s, err := NewServer(dir)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	defer s.Close()
	_ = s.store.AddPending(index.PendingNote{Type: "gotcha", Title: "Keep me", Do: "do x", Dont: "dont y", Why: "because"})
	_ = s.store.AddPending(index.PendingNote{Type: "decision", Title: "Toss me"})
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
