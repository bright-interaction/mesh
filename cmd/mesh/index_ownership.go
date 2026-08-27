// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/bright-interaction/mesh/internal/index"
)

type ownerCatchUpWait func(context.Context, *index.Store, string, time.Duration) error
type oneShotIndexReconcile func(*index.Store, string) (index.Reconciliation, error)

// openOneShotCurrent claims an otherwise idle vault for a bounded auxiliary-table
// mutation. Unlike a declared daemon it never preempts a live MCP/watch owner; unlike a
// transient filesystem reconcile it cannot itself be preempted halfway through the DB
// operation. Callers must invoke the returned close function.
func openOneShotCurrent(vaultDir, role string) (*index.Store, func(), error) {
	lock, err := index.AcquireOneShotOwnerLock(filepath.Join(vaultDir, ".mesh"), role)
	if err != nil {
		return nil, func() {}, err
	}
	store, err := index.OpenCurrentOwned(vaultDir, lock)
	if err != nil {
		_ = lock.Release()
		return nil, func() {}, err
	}
	return store, func() {
		_ = store.Close()
		_ = lock.Release()
	}, nil
}

// openOneShotCurrentOrReadOnly is for commands whose analysis is read-only but which
// opportunistically persist a cache when the vault is idle. Beside a live owner they
// compute from a physical read-only Store instead of refusing a healthy watched vault.
func openOneShotCurrentOrReadOnly(vaultDir, role string) (*index.Store, bool, func(), error) {
	store, closeStore, err := openOneShotCurrent(vaultDir, role)
	if err == nil {
		return store, true, closeStore, nil
	}
	if !errors.Is(err, index.ErrOwnerHeld) {
		return nil, false, func() {}, err
	}
	store, err = index.OpenReadOnly(vaultDir)
	if err != nil {
		return nil, false, func() {}, err
	}
	return store, false, func() { _ = store.Close() }, nil
}

func openOneShotRebuild(vaultDir, role string) (*index.Store, bool, func(), error) {
	lock, err := index.AcquireOneShotOwnerLock(filepath.Join(vaultDir, ".mesh"), role)
	if err != nil {
		return nil, false, func() {}, err
	}
	store, recovered, err := index.OpenRebuildOwned(vaultDir, lock)
	if err != nil {
		_ = lock.Release()
		return nil, false, func() {}, err
	}
	return store, recovered, func() {
		_ = store.Close()
		_ = lock.Release()
	}, nil
}

// reconcileOneShotThroughOwner reconciles after a bounded filesystem/network command
// without becoming a second index writer. A transient, preemptible claim is deliberately
// used: it may claim an idle vault, but it cannot preempt either an elected MCP owner or
// a declared watcher. Beside either owner it opens read-only and waits for that owner to
// absorb the bytes already written to disk.
func reconcileOneShotThroughOwner(ctx context.Context, vaultDir, role string) error {
	return reconcileOneShotThroughOwnerWithWait(ctx, vaultDir, role,
		func(ctx context.Context, store *index.Store, root string, bound time.Duration) error {
			return store.AwaitOwnerCaughtUp(ctx, root, bound)
		})
}

func reconcileOneShotThroughOwnerWithWait(ctx context.Context, vaultDir, role string, wait ownerCatchUpWait) error {
	return reconcileOneShotThroughOwnerWithHooks(ctx, vaultDir, role, wait, index.Reconcile)
}

func reconcileOneShotThroughOwnerWithHooks(ctx context.Context, vaultDir, role string, wait ownerCatchUpWait, reconcile oneShotIndexReconcile) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	lock, err := index.AcquireOwnerLock(filepath.Join(vaultDir, ".mesh"), role, true)
	if errors.Is(err, index.ErrOwnerHeld) {
		store, oerr := index.OpenReadOnly(vaultDir)
		if oerr != nil {
			return oerr
		}
		defer store.Close()
		return wait(ctx, store, vaultDir, index.OwnerIndexBound)
	}
	if err != nil {
		return err
	}

	store, err := index.OpenOwned(vaultDir, lock)
	if err != nil {
		lostOwnership := !lock.Held()
		lerr := lock.Release()
		if !lostOwnership {
			return errors.Join(err, lerr)
		}
		// OpenOwned refused because a declared owner took over. If that winner has
		// already opened the index, wait through it;
		// otherwise return the loud not-queryable error rather than attempting a
		// second writable open.
		reader, oerr := index.OpenReadOnly(vaultDir)
		if oerr != nil {
			return errors.Join(err, lerr, oerr)
		}
		defer reader.Close()
		return errors.Join(lerr, wait(ctx, reader, vaultDir, index.OwnerIndexBound))
	}
	_, rerr := reconcile(store, vaultDir)
	// A declared watcher may take this deliberately preemptible claim while the bounded
	// pass is running. If it did, wait for the winner's authoritative state before
	// reporting the index caught up.
	lostOwnership := !lock.Held()
	cerr := store.Close()
	lerr := lock.Release()
	if !lostOwnership {
		return errors.Join(rerr, cerr, lerr)
	}
	if rerr != nil && !errors.Is(rerr, index.ErrReadOnly) {
		return errors.Join(rerr, cerr, lerr)
	}
	reader, err := index.OpenReadOnly(vaultDir)
	if err != nil {
		return errors.Join(cerr, lerr, err)
	}
	defer reader.Close()
	// ErrReadOnly is the expected signal that the declared winner displaced this
	// transient pass at a write boundary. Its durable filesystem work still needs the
	// winner catch-up wait; cleanup failures remain loud alongside that result.
	return errors.Join(cerr, lerr, wait(ctx, reader, vaultDir, index.OwnerIndexBound))
}
