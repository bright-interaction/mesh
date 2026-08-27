// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/bright-interaction/mesh/internal/vault"
)

// Waiting for the single owning writer.
//
// Every read-only surface (a `mesh mcp` window that did not win the owner election, the
// web viewer) publishes a
// change the same way: put the durable artifact on disk, then wait for the owner to
// absorb it, then re-read. These are the mechanics of that wait, here rather than in
// one surface, because the two surfaces answer the same question and a fix that lands
// on only one of them is the recurring defect shape in this estate.
//
// The polls are indexed point queries on the read pool. WAL readers never block the
// writer, and QueryRow closes its rows, so none of this can pin a read snapshot the way
// a long-lived *sql.Rows would.

// ErrOwnerNotIndexing means the single owning writer did not absorb a durable change
// inside the bound. The change IS on disk; only its indexing is missing, which is a
// liveness failure of the owner (`mesh watch` / `mesh sync --watch` stopped), not a
// durability failure of the write. Callers must say exactly that, because the one thing
// that must never happen here is reporting a failed write for something that exists:
// the agent retries and Mesh mints a near-duplicate.
var ErrOwnerNotIndexing = errors.New("the change was saved but the owning writer did not apply it in time; it is NOT queryable yet")

const (
	// OwnerIndexPollInterval is how often a read-only surface checks whether the owner
	// has landed a change.
	OwnerIndexPollInterval = 50 * time.Millisecond
	// OwnerIndexBound bounds that wait. MEASURED, not guessed. Against a live owner at
	// production cadence (300ms debounce, 8s periodic sweep), write-to-queryable over 12
	// samples was p50 310ms, max 328ms across repeated runs: essentially the debounce,
	// because fsnotify and not the sweep is what delivers the note. See
	// TestWriteBackLatencyDistribution and TestWriteBackRidesFsnotifyNotThePeriodicSweep.
	//
	// Those samples are a tiny vault, where the reconcile itself is ~0. On the reference
	// vault a reconcile has been measured at 1.2 to 2.3s, so the realistic worst case is
	// debounce plus reindex, call it ~2.6s. 10s is roughly 4x that, which also absorbs an
	// owner already mid-reindex of a larger change.
	//
	// Two measured ways a note misses its fsnotify event and falls through to the
	// PERIODIC sweep instead:
	//   - the moment right after the owner starts, before its watches are registered;
	//   - a burst against a SHORT debounce, where the watcher is mid-reconcile. A
	//     50ms-debounce owner put p50 at its 500ms sweep rather than at the 58ms its
	//     fastest sample proved was possible. The 300ms production debounce showed none
	//     of this over repeated runs.
	// Such a change is still durable and does become queryable, just later than this
	// bound, so it is reported as not-yet-queryable rather than silently accepted. That
	// is also why the periodic sweep is pinned UNDER this bound (see cmd/mesh): a sweep
	// interval above it is precisely the case where a healthy owner reads as down.
	OwnerIndexBound = 10 * time.Second
)

// AwaitNoteIndexed blocks until the owning writer has indexed noteID.
//
// It needs no IPC: vault.CreateNote already wrote the file, the owner's fsnotify sees it
// inside its debounce, and its reconcile commits the note row, the FTS row and the graph
// tables in ONE transaction (IndexVaultIncremental). That atomicity is what makes polling
// the note row a valid readiness signal for the graph: if NotePath sees it, LoadGraph
// will too.
func (s *Store) AwaitNoteIndexed(ctx context.Context, noteID string, timeout time.Duration) error {
	return s.awaitOwner(ctx, timeout, func(probeCtx context.Context) (bool, error) {
		_, err := s.NotePathContext(probeCtx, noteID)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, sql.ErrNoRows):
			return false, nil // not landed yet
		default:
			return false, err // a real read error
		}
	})
}

// AwaitOpApplied blocks until the owning writer has applied a queued op (see ops.go).
// The op file is removed only after its effect is committed, so its absence is the
// readiness signal, exactly as the note row is for a written note.
func (s *Store) AwaitOpApplied(ctx context.Context, opName string, timeout time.Duration) error {
	if opName == "" {
		return nil
	}
	return s.awaitOwner(ctx, timeout, func(probeCtx context.Context) (bool, error) {
		queued, err := s.OpQueuedContext(probeCtx, opName)
		return !queued, err
	})
}

// AwaitOwnerCaughtUp waits for the owning writer to absorb whatever the vault has
// drifted by. It reports stale=true (with ErrOwnerNotIndexing) when the drift outlived
// the bound, so a caller can report the index as behind instead of claiming a reindex it
// never performed.
func (s *Store) AwaitOwnerCaughtUp(ctx context.Context, vaultRoot string, timeout time.Duration) error {
	return s.awaitOwner(ctx, timeout, func(probeCtx context.Context) (bool, error) {
		d, err := s.PendingDriftContext(probeCtx, vaultRoot)
		if err != nil {
			return false, err
		}
		return !d.Any(), nil
	})
}

// awaitOwner polls done until it reports true, the deadline passes (ErrOwnerNotIndexing)
// or ctx is cancelled. The first check happens BEFORE any sleep, so an already-satisfied
// wait costs one query rather than a poll interval.
func (s *Store) awaitOwner(ctx context.Context, timeout time.Duration, done func(context.Context) (bool, error)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if timeout <= 0 {
		timeout = OwnerIndexBound
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	probeErr := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if probeCtx.Err() != nil {
			return ErrOwnerNotIndexing
		}
		return nil
	}
	for {
		if err := probeErr(); err != nil {
			return err
		}
		ok, err := done(probeCtx)
		if waitErr := probeErr(); waitErr != nil {
			return waitErr
		}
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-probeCtx.Done():
			return probeErr()
		case <-time.After(OwnerIndexPollInterval):
		}
	}
}

// OpQueuedContext is the context-aware readiness probe used by read-only callers that
// queued bookkeeping for the owner. Stat has no context in the standard library, so the
// read-only result is isolated by vault.StatContext and may finish late only in its
// private worker.
func (s *Store) OpQueuedContext(ctx context.Context, name string) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if name == "" {
		return false, nil
	}
	_, err := vault.StatContext(ctx, filepath.Join(OpsDir(s.dir), name))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// PendingDrift is the part of the vault the owning writer has not absorbed yet and
// still can: notes added, changed or removed on disk that the index does not reflect.
// DriftReport resolves duplicate-id ownership and skips currently unparseable files,
// so the resulting set already excludes notes an index pass would deliberately drop.
// It must not consult the previous pass's dropped-note record: that record has no
// content fingerprint and would hide a formerly broken note after the user fixed it.
func (s *Store) PendingDrift(vaultRoot string) (Drift, error) {
	return s.PendingDriftContext(context.Background(), vaultRoot)
}

// PendingDriftContext is PendingDrift with cancellation across the vault scan and note
// reads.
func (s *Store) PendingDriftContext(ctx context.Context, vaultRoot string) (Drift, error) {
	return s.DriftReportContext(ctx, vaultRoot)
}
