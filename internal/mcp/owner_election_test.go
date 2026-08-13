// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
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
