// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/index"
)

// TestMCPElectsItselfOwnerAndWriteBackIsQueryable is the regression guard for the shipped
// configuration.
//
// What a new user gets is one line of JSON: `mesh mcp --vault <path>`, and nothing else.
// With the MCP server opening the index read-only, no process anywhere was indexing that
// vault, so every mesh_append_note sat out the full owner bound, came back with
// index_stale + owner_down, and left the note unqueryable. The flywheel was off for
// everyone who followed the documented setup.
//
// So: on a vault nobody owns, the server must elect itself the owning writer and a
// write-back must be queryable the moment the call returns, with no daemon and no manual
// `mesh index`.
func TestMCPElectsItselfOwnerAndWriteBackIsQueryable(t *testing.T) {
	dir := t.TempDir()
	seedVaultFiles(t, dir)
	seedIndex(t, dir)

	srv, err := NewOwningServer(dir, "mesh mcp (test)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	if err := srv.WaitReady(); err != nil {
		t.Fatal(err)
	}
	// Keep the bound short: if this server is NOT the owner, nothing else will ever land
	// the note, and the point is to fail on the assertions rather than to wait 10s first.
	srv.ownerIndexTimeout = 500 * time.Millisecond

	if !srv.OwnsIndex() {
		t.Fatal("mesh mcp did not take the owning-writer role on a vault nobody owns, so nothing " +
			"will index this vault and every write-back will report owner_down")
	}
	info, live := index.OwnerStatus(filepath.Join(dir, ".mesh"))
	if !live || info.PID != os.Getpid() {
		t.Fatalf("the vault's owner claim does not name this process: live=%v info=%+v", live, info)
	}

	out := writeNoteVia(t, srv, "a shipped mcp config indexes its own write-back")
	if out["index_stale"] == true || out["owner_down"] == true {
		t.Fatalf("the elected owner failed to index its own write-back: %v", out)
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("no id in write-back result: %v", out)
	}
	if _, err := srv.store.NotePath(id); err != nil {
		t.Fatalf("note %q is not queryable straight after the write-back: %v", id, err)
	}

	// The other half of the same defect: a note the user edits in their editor. A
	// read-only server's mesh_reindex can only re-read what an owner persisted, so with
	// no owner it waited out the bound and reported the index stale while the note sat
	// on disk. The owner indexes it.
	edited := filepath.Join(dir, "decisions", "edited-by-hand.md")
	if err := os.WriteFile(edited,
		[]byte("---\nid: edited-by-hand\ntype: decision\nwhen: 2026-01-01\ndo: x\ndont: y\nwhy: typed in an editor\n---\n# Edited\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	res, rerr := srv.handleToolsCall(WithLocalOperator(context.Background()),
		json.RawMessage(`{"name":"mesh_reindex","arguments":{}}`))
	if rerr != nil {
		t.Fatalf("mesh_reindex: %+v", rerr)
	}
	payload := decodeToolPayload(t, res)
	if payload["index_stale"] == true {
		t.Fatalf("mesh_reindex could not index a hand-edited note: %v", payload)
	}
	if _, err := srv.store.NotePath("edited-by-hand"); err != nil {
		t.Fatalf("a note edited in the user's editor never became searchable: %v", err)
	}
}

// TestMCPYieldsToARunningOwner: election must not reintroduce the multi-writer contention
// the single-writer split removed. Beside a live owner, the server reads.
func TestMCPYieldsToARunningOwner(t *testing.T) {
	dir := t.TempDir()
	seedVaultFiles(t, dir)
	seedIndex(t, dir)

	lock, err := index.AcquireOwnerLock(filepath.Join(dir, ".mesh"), "mesh watch (test)", false)
	if err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	t.Cleanup(func() { lock.Release() })

	srv, err := NewOwningServer(dir, "mesh mcp (test)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	if err := srv.WaitReady(); err != nil {
		t.Fatal(err)
	}
	if srv.OwnsIndex() {
		t.Fatal("mesh mcp took the vault from a running owner; two long-lived writers against " +
			"one mesh.db is the contention the single-writer split exists to prevent")
	}
	if !srv.store.ReadOnly() {
		t.Error("a server that lost the election still opened a writable store")
	}
}

func TestMCPElectionRetriesWhenOwnerExitsAfterReadOnlyOpen(t *testing.T) {
	dir := t.TempDir()
	seedVaultFiles(t, dir)
	seedIndex(t, dir)
	declared, err := index.AcquireOwnerLock(filepath.Join(dir, ".mesh"), "mesh watch (test)", false)
	if err != nil {
		t.Fatal(err)
	}
	afterMCPReadOnlyOpen = func() {
		afterMCPReadOnlyOpen = nil
		if rerr := declared.Release(); rerr != nil {
			t.Errorf("release startup owner: %v", rerr)
		}
	}
	t.Cleanup(func() {
		afterMCPReadOnlyOpen = nil
		_ = declared.Release()
	})

	srv, err := NewOwningServer(dir, "mesh mcp (test)")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if !srv.OwnsIndex() {
		t.Fatal("owner exited after read-only open and MCP remained a permanently ownerless reader")
	}
}

func TestMCPElectionFallsBackWhenDeclaredOwnerPreemptsBeforeOwnedOpen(t *testing.T) {
	dir := t.TempDir()
	seedVaultFiles(t, dir)
	seedIndex(t, dir)
	var declared *index.OwnerLock
	beforeMCPOwnedOpen = func(*index.OwnerLock) {
		beforeMCPOwnedOpen = nil
		var aerr error
		declared, aerr = index.AcquireOwnerLock(filepath.Join(dir, ".mesh"), "mesh watch (test)", false)
		if aerr != nil {
			t.Errorf("declared takeover: %v", aerr)
		}
	}
	t.Cleanup(func() {
		beforeMCPOwnedOpen = nil
		if declared != nil {
			_ = declared.Release()
		}
	})

	srv, err := NewOwningServer(dir, "mesh mcp (test)")
	if err != nil {
		t.Fatalf("turnover during owned open should fall back read-only, not abort startup: %v", err)
	}
	defer srv.Close()
	if srv.OwnsIndex() || !srv.store.ReadOnly() {
		t.Fatal("MCP did not fall back to the declared owner after pre-open takeover")
	}
}

// TestADeclaredOwnerPreemptsTheMCPClaim: the operator's explicit `mesh watch` wins over an
// opportunistic MCP claim, and the demoted server stops behaving like the owner at once,
// rather than both of them reindexing the same vault.
func TestADeclaredOwnerPreemptsTheMCPClaim(t *testing.T) {
	dir := t.TempDir()
	seedVaultFiles(t, dir)
	seedIndex(t, dir)

	srv, err := NewOwningServer(dir, "mesh mcp (test)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	if err := srv.WaitReady(); err != nil {
		t.Fatal(err)
	}
	if !srv.OwnsIndex() {
		t.Fatal("the mcp server should own an unowned vault")
	}

	lock, err := index.AcquireOwnerLock(filepath.Join(dir, ".mesh"), "mesh watch (test)", false)
	if err != nil {
		t.Fatalf("a declared owner must be able to take a preemptible claim: %v", err)
	}
	t.Cleanup(func() { lock.Release() })

	if srv.OwnsIndex() {
		t.Fatal("the mcp server still considers itself the owner after `mesh watch` took the claim")
	}
	if !srv.store.ReadOnly() {
		t.Fatal("the preempted server still advertises a writable Store")
	}
	if err := srv.store.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO metrics(key,value) VALUES('preempted-write',1)`)
		return err
	}); !errors.Is(err, index.ErrReadOnly) {
		t.Fatalf("preempted Store.Write = %v, want ErrReadOnly", err)
	}
	// Telemetry runs on the Store's background writer rather than an MCP ownership
	// branch. It must be forwarded to the new owner, not committed by the old one.
	_ = srv.store.IncrMetric("preempted-telemetry", 1)
	if got, err := srv.store.Metric("preempted-telemetry"); err != nil || got != 0 {
		t.Fatalf("preempted writer committed telemetry: value=%d err=%v", got, err)
	}
}

func TestPreemptedMCPRecoversIndexingAfterDeclaredOwnerExits(t *testing.T) {
	dir := t.TempDir()
	seedVaultFiles(t, dir)
	seedIndex(t, dir)
	srv, err := NewOwningServer(dir, "mesh mcp (test)")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if err := srv.WaitReady(); err != nil {
		t.Fatal(err)
	}
	declared, err := index.AcquireOwnerLock(filepath.Join(dir, ".mesh"), "mesh watch (test)", false)
	if err != nil {
		t.Fatal(err)
	}
	if srv.OwnsIndex() {
		t.Fatal("declared owner did not preempt MCP")
	}
	if err := declared.Release(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "recovered.md"), []byte("---\nid: recovered\ntype: note\n---\n# Recovered\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := srv.reconcileOnce(true); err != nil {
		t.Fatalf("first pass after temporary owner exit did not recover indexing: %v", err)
	}
	if _, err := srv.store.NotePath("recovered"); err != nil {
		t.Fatalf("editor note remained invisible after owner exited: %v", err)
	}
}

func TestMCPPreemptedDuringAsyncInitialReloadRefreshesWinner(t *testing.T) {
	dir := t.TempDir()
	seedVaultFiles(t, dir)
	seedIndex(t, dir)
	var declared *index.OwnerLock
	beforeMCPInitialReload = func() {
		beforeMCPInitialReload = nil
		var aerr error
		declared, aerr = index.AcquireOwnerLock(filepath.Join(dir, ".mesh"), "mesh watch (test)", false)
		if aerr != nil {
			t.Errorf("preempt during initial reload: %v", aerr)
		}
	}
	t.Cleanup(func() {
		beforeMCPInitialReload = nil
		if declared != nil {
			_ = declared.Release()
		}
	})

	srv, err := NewOwningServer(dir, "mesh mcp (test)")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if err := srv.WaitReady(); err != nil {
		t.Fatalf("expected ownership displacement to refresh the winner, got permanent readyErr: %v", err)
	}
	if srv.OwnsIndex() {
		t.Fatal("MCP still claims ownership after declared startup takeover")
	}
	if _, err := srv.store.NotePath("sqlite"); err != nil {
		t.Fatalf("winner's existing index was not queryable after startup fallback: %v", err)
	}
}

func TestMCPInitialReloadRecoversWhenWinnerExitsDuringCatchUpWait(t *testing.T) {
	dir := t.TempDir()
	seedVaultFiles(t, dir)
	seedIndex(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "winner-exited.md"), []byte("---\nid: winner-exited\ntype: note\n---\n# Winner exited\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	meshDir := filepath.Join(dir, ".mesh")
	initial, err := index.AcquireOwnerLock(meshDir, "mesh mcp (test)", true)
	if err != nil {
		t.Fatal(err)
	}
	store, err := index.OpenOwned(dir, initial)
	if err != nil {
		_ = initial.Release()
		t.Fatal(err)
	}
	var winner *index.OwnerLock
	beforeMCPInitialReload = func() {
		beforeMCPInitialReload = nil
		var aerr error
		winner, aerr = index.AcquireOwnerLock(meshDir, "mesh watch (test)", false)
		if aerr != nil {
			t.Errorf("preempt initial MCP owner: %v", aerr)
		}
	}
	beforeMCPAwaitOwnerCaughtUp = func() {
		beforeMCPAwaitOwnerCaughtUp = nil
		if winner == nil {
			t.Error("catch-up wait began without the declared winner")
			return
		}
		if rerr := winner.Release(); rerr != nil {
			t.Errorf("release winner during catch-up: %v", rerr)
		}
	}
	t.Cleanup(func() {
		beforeMCPInitialReload = nil
		beforeMCPAwaitOwnerCaughtUp = nil
		if winner != nil {
			_ = winner.Release()
		}
	})

	srv, err := newServerWithStoreTimeout(dir, store, initial, "mesh mcp (test)", 100*time.Millisecond)
	if err != nil {
		_ = store.Close()
		_ = initial.Release()
		t.Fatal(err)
	}
	defer srv.Close()
	if err := srv.WaitReady(); err != nil {
		t.Fatalf("winner exited during startup catch-up and MCP permanently sealed readyErr: %v", err)
	}
	if _, err := srv.store.NotePath("winner-exited"); err != nil {
		t.Fatalf("MCP did not recover and index drift after the startup winner exited: %v", err)
	}
}

func TestMCPPollsOpsBeforeStartupEnrichmentFinishes(t *testing.T) {
	dir := t.TempDir()
	seedVaultFiles(t, dir)
	seedIndex(t, dir)
	enrichmentStarted := make(chan struct{})
	releaseEnrichment := make(chan struct{})
	beforeMCPEnrichment = func() {
		close(enrichmentStarted)
		<-releaseEnrichment
	}
	t.Cleanup(func() { beforeMCPEnrichment = nil })

	srv, err := NewOwningServer(dir, "mesh mcp no-watch (test)")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	select {
	case <-enrichmentStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("initial load never reached enrichment")
	}
	reader, err := index.OpenReadOnly(dir)
	if err != nil {
		close(releaseEnrichment)
		t.Fatal(err)
	}
	defer reader.Close()
	pending := index.PendingNote{Type: "gotcha", Title: "Poll before enrichment", Do: "start the op poller at readiness"}
	name, err := reader.EnqueueOp(index.Op{Kind: index.OpAddPending, Pending: &pending})
	if err != nil {
		close(releaseEnrichment)
		t.Fatal(err)
	}
	srv.opWake <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	werr := reader.AwaitOpApplied(ctx, name, 500*time.Millisecond)
	cancel()
	close(releaseEnrichment)
	if rerr := srv.WaitReady(); rerr != nil {
		t.Fatal(rerr)
	}
	if werr != nil {
		t.Fatalf("op polling was delayed until startup enrichment completed: %v", werr)
	}
}

func TestOwningMCPPollsPendingOpsWithoutWatch(t *testing.T) {
	dir := t.TempDir()
	seedVaultFiles(t, dir)
	seedIndex(t, dir)
	srv, err := NewOwningServer(dir, "mesh mcp no-watch (test)")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if err := srv.WaitReady(); err != nil {
		t.Fatal(err)
	}
	reader, err := index.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	pending := index.PendingNote{Type: "gotcha", Title: "No-watch owner drains extraction", Do: "poll .mesh/ops"}
	name, err := reader.EnqueueOp(index.Op{Kind: index.OpAddPending, Pending: &pending})
	if err != nil {
		t.Fatal(err)
	}
	// Wake the same background select the production ticker drives. This makes the
	// regression deterministic without weakening the real cross-process polling path.
	srv.opWake <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := reader.AwaitOpApplied(ctx, name, 2*time.Second); err != nil {
		t.Fatalf("no-watch MCP left the automatic extraction op queued: %v", err)
	}
	got, err := reader.GetPending(index.PendingID(pending.Type, pending.Title))
	if err != nil || got.Do != pending.Do {
		t.Fatalf("no-watch owner did not commit pending item: got=%+v err=%v", got, err)
	}
}

func TestOwningMCPWatchReconcileDrainsPendingOps(t *testing.T) {
	dir := t.TempDir()
	seedVaultFiles(t, dir)
	seedIndex(t, dir)
	srv, err := NewOwningServer(dir, "mesh mcp --watch (test)")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if err := srv.WaitReady(); err != nil {
		t.Fatal(err)
	}
	reader, err := index.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	pending := index.PendingNote{Type: "decision", Title: "Watch pass drains extraction", Do: "drain before reconcile"}
	name, err := reader.EnqueueOp(index.Op{Kind: index.OpAddPending, Pending: &pending})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.reconcileOnce(false); err != nil {
		t.Fatal(err)
	}
	if reader.OpQueued(name) {
		t.Fatal("watch reconcile returned while its queued pending upsert remained")
	}
	if _, err := reader.GetPending(index.PendingID(pending.Type, pending.Title)); err != nil {
		t.Fatalf("watch reconcile did not apply pending upsert: %v", err)
	}
}

// TestHealthReportsAVaultWithNoOwningWriter: mesh_health is where an agent finds out its
// vault is broken, and "nothing is indexing this" was the one condition it could not
// report. Every note written while it holds is invisible to search.
func TestHealthReportsAVaultWithNoOwningWriter(t *testing.T) {
	srv := newTestServer(t) // read-only, and no owner anywhere

	res, rerr := srv.toolHealth(WithLocalOperator(context.Background()), json.RawMessage(`{}`))
	if rerr != nil {
		t.Fatalf("mesh_health: %+v", rerr)
	}
	payload := decodeToolPayload(t, res)
	counts, _ := payload["counts"].(map[string]any)
	if counts[index.NoOwnerIssue] != float64(1) {
		t.Fatalf("mesh_health did not report the missing owning writer: %v", payload)
	}
	var found bool
	for _, f := range asList(payload["findings"]) {
		fm, _ := f.(map[string]any)
		if fm["issue"] != index.NoOwnerIssue {
			continue
		}
		found = true
		detail, _ := fm["detail"].(string)
		// A stranger has to be able to act on it, so the finding names the commands.
		for _, want := range []string{"mesh mcp", "mesh watch"} {
			if !containsStr(detail, want) {
				t.Errorf("the no-owner finding does not name %q: %q", want, detail)
			}
		}
	}
	if !found {
		t.Fatalf("no no-owner finding in %v", payload)
	}

	// With an owner running it must go quiet, or it is noise every healthy vault carries.
	lock, err := index.AcquireOwnerLock(filepath.Join(srv.vaultRoot, ".mesh"), "mesh watch (test)", false)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	res, rerr = srv.toolHealth(WithLocalOperator(context.Background()), json.RawMessage(`{}`))
	if rerr != nil {
		t.Fatalf("mesh_health: %+v", rerr)
	}
	payload = decodeToolPayload(t, res)
	counts, _ = payload["counts"].(map[string]any)
	if _, still := counts[index.NoOwnerIssue]; still {
		t.Fatalf("mesh_health reports no owner while one is running: %v", payload)
	}
}
