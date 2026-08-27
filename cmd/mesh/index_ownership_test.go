// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/spf13/cobra"
)

func TestOneShotReconcileWaitsReadOnlyBesideEveryLiveOwner(t *testing.T) {
	for _, tc := range []struct {
		name        string
		preemptible bool
	}{
		{name: "declared watcher", preemptible: false},
		{name: "elected MCP", preemptible: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, ".mesh"), 0o755); err != nil {
				t.Fatal(err)
			}
			owner, err := index.AcquireOwnerLock(filepath.Join(dir, ".mesh"), tc.name, tc.preemptible)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Release()
			ownerStore, err := index.OpenOwned(dir, owner)
			if err != nil {
				t.Fatal(err)
			}
			defer ownerStore.Close()
			if err := os.WriteFile(filepath.Join(dir, "owner-held.md"), []byte("---\nid: owner-held\ntype: note\n---\n# Owner held\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			waited := errors.New("waited through live owner")
			err = reconcileOneShotThroughOwnerWithWait(context.Background(), dir, "mesh sync", func(_ context.Context, store *index.Store, root string, bound time.Duration) error {
				if root != dir || bound != index.OwnerIndexBound {
					t.Fatalf("owner wait got root=%q bound=%s", root, bound)
				}
				if _, werr := index.Reconcile(store, root); !errors.Is(werr, index.ErrReadOnly) {
					t.Fatalf("owner-held route opened a writable second store: %v", werr)
				}
				return waited
			})
			if !errors.Is(err, waited) {
				t.Fatalf("one-shot reconcile did not route through the owner wait: %v", err)
			}
			if !owner.Held() {
				t.Fatal("one-shot reconcile preempted the live owner")
			}
		})
	}
}

func TestOneShotReconcileClaimsAndReleasesWhenNoOwnerExists(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes", "one-shot.md"), []byte("---\nid: one-shot\ntype: note\n---\n# One shot\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := reconcileOneShotThroughOwnerWithWait(context.Background(), dir, "mesh sync", func(context.Context, *index.Store, string, time.Duration) error {
		t.Fatal("no-owner path unexpectedly waited on another process")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if info, live := index.OwnerStatus(filepath.Join(dir, ".mesh")); live {
		t.Fatalf("transient owner was not released: %+v", info)
	}
	store, err := index.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.NotePath("one-shot"); err != nil {
		t.Fatalf("transient owner did not reconcile the note: %v", err)
	}
}

func TestOneShotReconcileWaitsForADeclaredOwnerThatTakesOverMidPass(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".mesh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "turnover.md"), []byte("---\nid: turnover\ntype: note\n---\n# Turnover\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var replacement *index.OwnerLock
	waited := false
	err := reconcileOneShotThroughOwnerWithHooks(context.Background(), dir, "mesh sync",
		func(_ context.Context, store *index.Store, root string, _ time.Duration) error {
			waited = true
			if replacement == nil || !replacement.Held() {
				t.Fatal("replacement owner was not held during catch-up wait")
			}
			if _, werr := index.Reconcile(store, root); !errors.Is(werr, index.ErrReadOnly) {
				t.Fatalf("turnover wait did not reopen read-only: %v", werr)
			}
			return nil
		},
		func(store *index.Store, root string) (index.Reconciliation, error) {
			var aerr error
			replacement, aerr = index.AcquireOwnerLock(filepath.Join(dir, ".mesh"), "mesh watch", false)
			if aerr != nil {
				return index.Reconciliation{}, aerr
			}
			// The takeover is observed at the actual Store write boundary, which is
			// how a scan in progress fails in production.
			return index.Reconcile(store, root)
		})
	t.Cleanup(func() {
		if replacement != nil {
			_ = replacement.Release()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !waited {
		t.Fatal("transient writer reported success without waiting for the owner that took over")
	}
}

func TestAuxiliaryOneShotWriterDoesNotPreemptLiveOwner(t *testing.T) {
	root := t.TempDir()
	seedStore, err := index.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := seedStore.Close(); err != nil {
		t.Fatal(err)
	}
	elected, err := index.AcquireOwnerLock(filepath.Join(root, ".mesh"), "mesh mcp", true)
	if err != nil {
		t.Fatal(err)
	}
	defer elected.Release()

	store, closeStore, err := openOneShotCurrent(root, "mesh health")
	defer closeStore()
	if store != nil || !errors.Is(err, index.ErrOwnerHeld) {
		t.Fatalf("auxiliary writer beside owner = store %v err %v, want ErrOwnerHeld", store, err)
	}
	if !elected.Held() {
		t.Fatal("auxiliary writer preempted the elected MCP owner")
	}
}

func TestAuxiliaryOneShotWriterRetainsAndReleasesIdleClaim(t *testing.T) {
	root := t.TempDir()
	seedStore, err := index.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := seedStore.Close(); err != nil {
		t.Fatal(err)
	}
	store, closeStore, err := openOneShotCurrent(root, "mesh health")
	if err != nil {
		t.Fatal(err)
	}
	if store.ReadOnly() {
		closeStore()
		t.Fatal("claimed auxiliary store is read-only")
	}
	info, live := index.OwnerStatus(filepath.Join(root, ".mesh"))
	if !live || info.Role != "mesh health" {
		closeStore()
		t.Fatalf("auxiliary writer did not retain claim: info=%+v live=%v", info, live)
	}
	closeStore()
	if _, live := index.OwnerStatus(filepath.Join(root, ".mesh")); live {
		t.Fatal("auxiliary writer left owner claim behind")
	}
}

func TestAuxiliaryAnalysisFallsBackReadOnlyBesideLiveOwner(t *testing.T) {
	root := t.TempDir()
	seedStore, err := index.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := seedStore.Close(); err != nil {
		t.Fatal(err)
	}
	owner, err := index.AcquireOwnerLock(filepath.Join(root, ".mesh"), "mesh watch", false)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Release()

	store, writable, closeStore, err := openOneShotCurrentOrReadOnly(root, "mesh health")
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore()
	if writable || !store.ReadOnly() {
		t.Fatal("analysis beside a live owner did not fall back to a physical read-only Store")
	}
	if !owner.Held() {
		t.Fatal("read-only analysis preempted the live owner")
	}
}

func TestExclusiveAuxiliaryCommandsTellOperatorToStopAndRestartOwner(t *testing.T) {
	root := t.TempDir()
	seedStore, err := index.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := seedStore.Close(); err != nil {
		t.Fatal(err)
	}
	owner, err := index.AcquireOwnerLock(filepath.Join(root, ".mesh"), "mesh mcp --watch", false)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Release()

	for _, tc := range []struct {
		name string
		cmd  *cobra.Command
		args []string
	}{
		{name: "embed", cmd: embedCmd(), args: []string{root, "--endpoint", "http://127.0.0.1:1/v1", "--model", "test"}},
		{name: "code reindex", cmd: codeReindexCmd(), args: []string{root}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runCLI(t, tc.cmd, tc.args...)
			if err == nil || !strings.Contains(err.Error(), "stop the live mesh mcp/watch owner") || !strings.Contains(err.Error(), "restart it") {
				t.Fatalf("owner-held UX = %v, want explicit stop/run/restart instructions", err)
			}
		})
	}
}
