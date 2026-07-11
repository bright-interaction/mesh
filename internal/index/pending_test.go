// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"fmt"
	"testing"
)

// The review queue is bounded: extraction is non-deterministic and can reword the same
// learning, so without a cap the queue could grow without limit. Adding past the cap
// prunes the oldest, keeping the newest pendingQueueCap items.
func TestPendingQueueCap(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Insert cap+50 distinct candidates with increasing created_at.
	for i := 0; i < pendingQueueCap+50; i++ {
		if err := store.AddPending(PendingNote{
			Type:      "gotcha",
			Title:     fmt.Sprintf("candidate number %d", i),
			Why:       "because",
			CreatedAt: int64(1000 + i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := store.PendingCount()
	if err != nil {
		t.Fatal(err)
	}
	if n != pendingQueueCap {
		t.Fatalf("PendingCount = %d, want %d (queue must be capped)", n, pendingQueueCap)
	}
	// The newest item must survive; the oldest must have been pruned.
	items, _ := store.ListPending()
	newestKept, oldestPruned := false, true
	for _, it := range items {
		if it.Title == fmt.Sprintf("candidate number %d", pendingQueueCap+49) {
			newestKept = true
		}
		if it.Title == "candidate number 0" {
			oldestPruned = false
		}
	}
	if !newestKept {
		t.Error("newest candidate was pruned; the cap should drop the OLDEST")
	}
	if !oldestPruned {
		t.Error("oldest candidate survived; the cap should have pruned it")
	}
}
