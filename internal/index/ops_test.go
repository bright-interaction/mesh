// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// opsVault is a vault with one note and an index an owner has already built, which is
// the only state a read-only store can be opened against.
func opsVault(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "n.md"),
		[]byte("---\nid: n\ntype: note\nwhen: 2026-01-01\n---\n# N\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	owner, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, _, err := ReindexFull(owner, dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A read-only store cannot delete a pending row, but it can queue the deletion and the
// owner applies it. This is the whole contract the web viewer's review queue rests on.
func TestOpQueueCarriesAWriteFromAReaderToTheOwner(t *testing.T) {
	dir := opsVault(t)

	seeder, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := seeder.AddPending(PendingNote{Type: "gotcha", Title: "Queue me"}); err != nil {
		t.Fatal(err)
	}
	id := PendingID("gotcha", "Queue me")
	seeder.Close()

	reader, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	// The direct write is refused, loudly and by sentinel, which is what makes queueing
	// the only honest route rather than a stylistic choice.
	if err := reader.DeletePending(id); err == nil {
		t.Fatal("a read-only store deleted a pending row")
	}
	name, err := reader.EnqueueOp(Op{Kind: OpDeletePending, ID: id})
	if err != nil {
		t.Fatal(err)
	}
	if !reader.OpQueued(name) {
		t.Fatal("the op is not queued right after being enqueued")
	}

	owner, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	n, err := owner.DrainOps()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("drained %d ops, want 1", n)
	}
	if _, err := owner.GetPending(id); err == nil {
		t.Fatal("the owner drained the queue but the pending row is still there")
	}
	if reader.OpQueued(name) {
		t.Fatal("the op file survived its own application; the reader would wait forever")
	}
}

func TestOpQueueCarriesPendingUpsertFromReaderToOwner(t *testing.T) {
	dir := opsVault(t)
	reader, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	pending := PendingNote{Type: "gotcha", Title: "Queued by extraction", Do: "route it", Source: "session.jsonl"}
	name, err := reader.EnqueueOp(Op{Kind: OpAddPending, Pending: &pending})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err := owner.DrainOps(); err != nil {
		t.Fatal(err)
	}
	got, err := owner.GetPending(PendingID(pending.Type, pending.Title))
	if err != nil || got.Do != pending.Do || got.Source != pending.Source {
		t.Fatalf("queued pending upsert did not land: got=%+v err=%v", got, err)
	}
	if reader.OpQueued(name) {
		t.Fatal("pending upsert op remained queued after commit")
	}
}

// The wait is the caller-facing half: it must return as soon as the owner applies the
// op, and report ErrOwnerNotIndexing (not success, not a generic error) when no owner
// ever does.
func TestAwaitOpApplied(t *testing.T) {
	dir := opsVault(t)
	reader, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	name, err := reader.EnqueueOp(Op{Kind: OpDeletePending, ID: "nothing"})
	if err != nil {
		t.Fatal(err)
	}
	// No owner: the wait must run out and say so.
	if err := reader.AwaitOpApplied(context.Background(), name, 150*time.Millisecond); err == nil {
		t.Fatal("waiting for an op no owner will apply returned success")
	} else if !strings.Contains(err.Error(), "NOT queryable yet") {
		t.Fatalf("want the owner-not-indexing sentinel, got %v", err)
	}

	// With an owner, the same wait returns as soon as the op lands.
	owner, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = owner.DrainOps()
	}()
	if err := reader.AwaitOpApplied(context.Background(), name, 5*time.Second); err != nil {
		t.Fatalf("the owner applied the op but the wait still failed: %v", err)
	}
}

// One unapplyable file must not block every op behind it. Drop what can never succeed,
// keep what can.
func TestDrainOpsDropsWhatCanNeverApply(t *testing.T) {
	dir := opsVault(t)
	seeder, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := seeder.AddPending(PendingNote{Type: "gotcha", Title: "Behind the bad one"}); err != nil {
		t.Fatal(err)
	}
	id := PendingID("gotcha", "Behind the bad one")
	seeder.Close()

	opsDir := OpsDir(filepath.Join(dir, ".mesh"))
	if err := os.MkdirAll(opsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Oldest first: a malformed file, an unknown kind, then a real op behind both.
	if err := os.WriteFile(filepath.Join(opsDir, "00000000000000000001-aaaa.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(Op{Kind: "from_a_future_version", ID: "x"})
	if err := os.WriteFile(filepath.Join(opsDir, "00000000000000000002-bbbb.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(Op{Kind: OpDeletePending, ID: id})
	if err := os.WriteFile(filepath.Join(opsDir, "00000000000000000003-cccc.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	owner, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err := owner.DrainOps(); err != nil {
		t.Fatalf("a bad op file failed the whole drain: %v", err)
	}
	if _, err := owner.GetPending(id); err == nil {
		t.Fatal("the good op behind the bad ones never ran")
	}
	left, err := os.ReadDir(opsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("the queue still holds %d file(s); a permanently bad op must be dropped, not retried forever", len(left))
	}
}

// Usage counters are generated only by read-only processes now (every mesh mcp window,
// the web viewer). If they stop reaching the index the flywheel measurement quietly
// dies, so this asserts the whole path: record on a reader, drain on the owner, see it
// in the metrics table.
func TestReadOnlyTelemetryReachesTheIndexThroughTheOwner(t *testing.T) {
	dir := opsVault(t)
	reader, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.IncrMetric("queries", 3); err != nil {
		t.Fatal(err)
	}
	if err := reader.IncrMetric("fetches", 1); err != nil {
		t.Fatal(err)
	}
	// Close forces the final flush, so this does not depend on the 2s ticker.
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}

	owner, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err := owner.DrainOps(); err != nil {
		t.Fatal(err)
	}
	if got, _ := owner.Metric("queries"); got != 3 {
		t.Errorf("queries = %d, want 3: a read-only surface's counters never reached the index", got)
	}
	if got, _ := owner.Metric("fetches"); got != 1 {
		t.Errorf("fetches = %d, want 1", got)
	}
}

// The queue is durable, so a reader whose owner is dead can otherwise fill the disk one
// click at a time. At the cap the enqueue must FAIL rather than silently drop: the
// caller's contract is that it can tell the user whether the action will take effect.
func TestEnqueueRefusesPastTheCap(t *testing.T) {
	dir := opsVault(t)
	reader, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	opsDir := OpsDir(filepath.Join(dir, ".mesh"))
	if err := os.MkdirAll(opsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(Op{Kind: OpDeletePending, ID: "x"})
	for i := 0; i < opsQueueCap; i++ {
		// Distinct names; the exact stamps do not matter here, only the count.
		name := filepath.Join(opsDir, "00000000000000000000-"+padded(i)+".json")
		if err := os.WriteFile(name, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := reader.EnqueueOp(Op{Kind: OpDeletePending, ID: "one too many"}); err == nil {
		t.Fatal("the queue accepted an op past its cap")
	}
}

func padded(i int) string {
	s := ""
	for _, c := range []int{i / 1000 % 10, i / 100 % 10, i / 10 % 10, i % 10} {
		s += string(rune('0' + c))
	}
	return s
}
