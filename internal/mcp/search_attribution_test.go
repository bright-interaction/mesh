// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"testing"

	"github.com/bright-interaction/mesh/internal/retrieve"
)

func TestFirstFetchAfterSearchRecordsContentFreeRankAndRoute(t *testing.T) {
	dir := t.TempDir()
	seedVaultFiles(t, dir)
	seedIndex(t, dir)
	s, err := NewOwningServer(dir, "economics-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WaitReady(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	s.rememberSearch([]retrieve.Card{{NoteID: "sqlite"}, {NoteID: "note"}}, retrieve.Economics{Route: "model"})
	if _, rerr := s.toolFetch(context.Background(), mustJSON(map[string]any{"id": "note"})); rerr != nil {
		t.Fatalf("fetch: %v", rerr)
	}
	for key, want := range map[string]int64{
		"retrieval:search_to_fetch":   1,
		"retrieval:selected_rank:2":   1,
		"rerank:selected_route:model": 1,
	} {
		got, err := s.store.Metric(key)
		if err != nil || got != want {
			t.Fatalf("metric %s = %d,%v; want %d", key, got, err, want)
		}
	}

	// The second read belongs to normal browsing, not a second choice from the
	// same slate, so it must not be attributed again.
	if _, rerr := s.toolFetch(context.Background(), mustJSON(map[string]any{"id": "sqlite"})); rerr != nil {
		t.Fatalf("second fetch: %v", rerr)
	}
	if got, err := s.store.Metric("retrieval:search_to_fetch"); err != nil || got != 1 {
		t.Fatalf("same search attributed twice: %d,%v", got, err)
	}
}
