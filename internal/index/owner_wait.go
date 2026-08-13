// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Waiting for the single owning writer.
//
// Every read-only surface (the per-window `mesh mcp`, the web viewer) publishes a
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
	return s.awaitOwner(ctx, timeout, func() (bool, error) {
		_, err := s.NotePath(noteID)
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
	return s.awaitOwner(ctx, timeout, func() (bool, error) {
		return !s.OpQueued(opName), nil
	})
}

// AwaitOwnerCaughtUp waits for the owning writer to absorb whatever the vault has
// drifted by. It reports stale=true (with ErrOwnerNotIndexing) when the drift outlived
// the bound, so a caller can report the index as behind instead of claiming a reindex it
// never performed.
func (s *Store) AwaitOwnerCaughtUp(ctx context.Context, vaultRoot string, timeout time.Duration) error {
	return s.awaitOwner(ctx, timeout, func() (bool, error) {
		d, err := s.PendingDrift(vaultRoot)
		if err != nil {
			return false, err
		}
		return !d.Any(), nil
	})
}

// awaitOwner polls done until it reports true, the deadline passes (ErrOwnerNotIndexing)
// or ctx is cancelled. The first check happens BEFORE any sleep, so an already-satisfied
// wait costs one query rather than a poll interval.
func (s *Store) awaitOwner(ctx context.Context, timeout time.Duration, done func() (bool, error)) error {
	if timeout <= 0 {
		timeout = OwnerIndexBound
	}
	deadline := time.Now().Add(timeout)
	for {
		ok, err := done()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if time.Now().After(deadline) {
			return ErrOwnerNotIndexing
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(OwnerIndexPollInterval):
		}
	}
}

// PendingDrift is the part of the vault the owning writer has not absorbed yet and
// still can: notes added, changed or removed on disk that the index does not reflect.
//
// Files the owner recorded as DROPPED are excluded. A quarantined duplicate id parses
// fine but is deliberately absent from the notes table, so DriftReport calls it Added
// forever; counting it would make every wait on a vault with one such note run out the
// whole bound and then report a perfectly healthy owner as down. (An unparseable note
// never reaches the drift lists at all, so it needs no exclusion.) Both kinds are still
// reported, by mesh_health, which is where a note that needs a human belongs.
func (s *Store) PendingDrift(vaultRoot string) (Drift, error) {
	d, err := s.DriftReport(vaultRoot)
	if err != nil {
		return Drift{}, err
	}
	known, err := s.DroppedNotes()
	if err != nil {
		return Drift{}, err
	}
	dropped := map[string]bool{}
	for _, fe := range known {
		dropped[fe.Path] = true
	}
	if len(dropped) == 0 {
		return d, nil
	}
	keep := func(paths []string) []string {
		out := paths[:0]
		for _, p := range paths {
			if !dropped[p] {
				out = append(out, p)
			}
		}
		return out
	}
	d.Added, d.Changed, d.Removed = keep(d.Added), keep(d.Changed), keep(d.Removed)
	return d, nil
}
