// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bright-interaction/mesh/internal/extract"
	"github.com/bright-interaction/mesh/internal/index"
)

// knownInVault must return true for a candidate that restates an existing note (so the
// review queue does not re-surface knowledge the vault already has) and false for a
// genuinely new one. Exercises the full path: retriever over the vault + TitleSimilarity.
func TestKnownInVaultDedup(t *testing.T) {
	dir := t.TempDir()
	note := "---\nid: ssrf-tailscale\ntype: gotcha\n---\n# SSRF denylist must include 100.64.0.0/10 (Tailscale CGNAT)\nResolve the host and reject the Tailscale CGNAT range in SSRF denylists.\n"
	if err := os.WriteFile(filepath.Join(dir, "ssrf.md"), []byte(note), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rtr, closeRtr := buildVaultRetriever(store, dir)
	defer closeRtr()
	if rtr == nil {
		t.Fatal("retriever failed to build over the vault")
	}
	ctx := context.Background()

	dup := extract.Candidate{Type: "gotcha", Title: "SSRF denylists must include 100.64.0.0/10 for Tailscale", Do: "add the CGNAT range"}
	if known, of := knownInVault(ctx, rtr, dup); !known {
		t.Errorf("restatement not detected as known (matched %q)", of)
	}

	fresh := extract.Candidate{Type: "decision", Title: "Mollie webhooks require re-fetch, not signature verification", Do: "re-fetch the payment by id"}
	if known, of := knownInVault(ctx, rtr, fresh); known {
		t.Errorf("a genuinely new candidate was wrongly deduped (matched %q)", of)
	}
}

func TestNearDuplicatePending(t *testing.T) {
	existing := []index.PendingNote{
		{Type: "gotcha", Title: "Verify new credential works BEFORE invalidating the old one"},
	}
	if of, dup := nearDuplicatePending("Verify new credentials work BEFORE retiring old ones", existing); !dup {
		t.Errorf("a reworded restatement of a queued item should be caught (matched %q)", of)
	}
	if _, dup := nearDuplicatePending("Bun is the only package manager allowed", existing); dup {
		t.Error("an unrelated title must not be treated as a duplicate")
	}
}

// writeToPending must not re-queue a candidate that restates an item already in the
// review queue, even when the LLM reworded it (so reruns and recurring sessions cannot
// flood the queue), while still queueing genuinely new candidates. This is the fix for
// the flood the live round-trip test surfaced (LLM rephrasing dodged the exact-id dedup).
func TestWriteToPendingSuppressesQueuedDuplicates(t *testing.T) {
	dir := t.TempDir()
	store, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddPending(index.PendingNote{Type: "gotcha", Title: "Verify new credential works BEFORE invalidating the old one", Do: "probe first"}); err != nil {
		t.Fatal(err)
	}
	store.Close() // writeToPending opens its own handle

	cands := []extract.Candidate{
		{Type: "gotcha", Title: "Verify new credentials work BEFORE retiring old ones", Do: "probe first"}, // reworded duplicate
		{Type: "decision", Title: "Bun is the only package manager allowed", Do: "use bun"},                // genuinely new
	}
	if err := writeToPending(dir, "session.jsonl", cands); err != nil {
		t.Fatal(err)
	}

	store2, err := index.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	items, _ := store2.ListPending()
	if len(items) != 2 {
		t.Fatalf("queue should hold the baseline + the one fresh candidate = 2, got %d", len(items))
	}
	for _, it := range items {
		if it.Title == "Verify new credentials work BEFORE retiring old ones" {
			t.Error("the reworded duplicate was queued despite matching an existing item")
		}
	}
}
