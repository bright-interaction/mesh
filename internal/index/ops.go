// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The owner-routed op queue.
//
// Write-back solved this problem for NOTES: a read-only surface creates a note by
// writing a FILE, and the single owning writer indexes it. That works because the note
// has a durable artifact of its own to ride on. A handful of actions have no such
// artifact, because what they change IS index state: clearing a review-queue item,
// stamping a promoted note in the flywheel. Before the single-writer split those rode
// on whichever process happened to hold a writable store, which on the web dashboard
// meant a long-lived second writer.
//
// So they get an artifact too. Enqueue writes one small JSON file per op (a filesystem
// write, legal on a read-only store); the owning writer drains the directory on every
// reconcile and applies each op through the one write path. The caller then waits for
// the effect exactly the way write-back waits for a note to be indexed, on the same
// bound, and reports the same loud owner_down when the owner is not running.
//
// Ordering is by filename, which is nanosecond-stamped, so a promote's writeback stamp
// cannot be applied before the note it refers to was created. Nothing here is
// re-derivable from the vault, so an op file is only removed once its effect is
// committed.

// Op is one queued index mutation. Kept deliberately small: this is a queue for
// bookkeeping a reader could not do itself, not a general RPC channel.
type Op struct {
	Kind   string `json:"kind"`
	ID     string `json:"id,omitempty"`      // OpDeletePending: the pending id
	NoteID string `json:"note_id,omitempty"` // OpRecordWriteback: the created note id
	Source string `json:"source,omitempty"`  // OpRecordWriteback: provenance ("agent")
	At     int64  `json:"at"`                // unix seconds, for the operator reading the dir

	// OpTelemetry: one batch of usage counters, already accumulated in memory by the
	// reader that observed them.
	Counts map[string]int64 `json:"counts,omitempty"`
	Reuse  []ReuseEvent     `json:"reuse,omitempty"`
	// OpAddPending: the complete idempotent pending-note upsert. A pointer keeps
	// malformed/missing payloads distinguishable from a deliberately empty field.
	Pending *PendingNote `json:"pending,omitempty"`
}

// ReuseEvent is one note fetched in a later session than it was authored in: the atom
// the flywheel reuse rate is computed from. Timestamped when the fetch happened, not
// when the batch is applied, so a queued batch does not distort the gap check.
type ReuseEvent struct {
	NoteID string `json:"note_id"`
	GapSec int64  `json:"gap_sec"`
	At     int64  `json:"at"`
}

const (
	// OpDeletePending clears a review-queue item (promoted or discarded).
	OpDeletePending = "delete_pending"
	// OpRecordWriteback stamps a note in the flywheel. Recoverable in principle
	// (BackfillWritebacks is idempotent and would find the note later) but queued
	// anyway, so the dashboard does not have to wait for a backfill to tell the truth.
	OpRecordWriteback = "record_writeback"
	// OpTelemetry carries usage counters from a read-only reader. Every process that
	// generates them is read-only now, so without this route the flywheel measurement
	// simply stops.
	OpTelemetry = "telemetry"
	// OpAddPending carries automatic extraction into the review queue when the live
	// MCP/watch owner is the only process allowed to mutate SQLite.
	OpAddPending = "add_pending"
)

// opsQueueCap bounds the directory. A reader whose owner is dead can keep enqueuing
// forever otherwise: every click is durable and nothing drains it. At the cap the
// enqueue FAILS rather than silently dropping, because the caller's whole contract is
// that it can report honestly whether the action will take effect.
const opsQueueCap = 1000

// ErrOpsQueueFull is returned when the queue is at opsQueueCap. It means the owning
// writer has not drained in a very long time, which is the same diagnosis as
// owner_down and wants the same remedy.
var ErrOpsQueueFull = errors.New("mesh: the owning writer's op queue is full; it has not drained in a long time (is `mesh watch` / `mesh sync --watch` running?)")

// OpsDir is the queue directory for a given index dir.
func OpsDir(meshDir string) string { return filepath.Join(meshDir, "ops") }

// EnqueueOp durably records an op for the owning writer to apply. Legal on a read-only
// store: it writes a file, not the database. Returns the queued filename so a caller
// can wait for that specific op to disappear if it wants to.
func (s *Store) EnqueueOp(op Op) (string, error) {
	if strings.TrimSpace(op.Kind) == "" {
		return "", errors.New("mesh: an op needs a kind")
	}
	if op.At == 0 {
		op.At = time.Now().Unix()
	}
	dir := OpsDir(s.dir)
	// 0700 to agree with pkg/meshclient and the ingest state beside it. An op names a
	// note id and a review-queue id, which is vault content, not public bookkeeping.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if n, err := countOps(dir); err == nil && n >= opsQueueCap {
		return "", ErrOpsQueueFull
	}
	b, err := json.Marshal(op)
	if err != nil {
		return "", err
	}
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	// Nanosecond prefix so a lexical sort is a chronological sort, random suffix so two
	// processes enqueuing in the same nanosecond cannot collide.
	name := fmt.Sprintf("%020d-%s.json", time.Now().UnixNano(), hex.EncodeToString(suffix[:]))
	// Temp+rename: a torn op file would be undrainable, and the owner must never see a
	// half-written one. The rename is what publishes it.
	tmp, err := os.CreateTemp(dir, ".op-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(b)
	cerr := tmp.Chmod(0o600)
	serr := tmp.Sync()
	clerr := tmp.Close()
	if err := firstOpErr(werr, cerr, serr, clerr); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	// The directory entry needs its own fsync, after the rename: the op's whole value is
	// that it survives when the process that queued it does not, and a queued promote's
	// bookkeeping that evaporates on a power cut leaves a note in the vault that is
	// forever also in the review queue.
	syncDir(dir)
	return name, nil
}

// syncDir fsyncs a directory so a rename into it survives a power loss. Best effort:
// some filesystems refuse to open a directory for sync, and failing the enqueue over it
// would be worse than the risk it covers.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}

// OpQueued reports whether a queued op file is still waiting to be applied. This is the
// readiness signal a read-only caller polls, the exact counterpart of polling NotePath
// for a written note: the file is removed only after its effect is committed.
func (s *Store) OpQueued(name string) bool {
	if name == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(OpsDir(s.dir), name))
	return err == nil
}

// DrainOps applies every queued op, oldest first, and removes each file once its effect
// is committed. Only the owning writer may call it; a read-only store refuses, because
// silently doing nothing here would look exactly like a healthy drain.
//
// An op whose apply FAILS is left in place for the next pass (a transient DB error must
// not lose a promoted note's bookkeeping). An op that can never succeed (unreadable,
// malformed, unknown kind) is removed and logged, so one bad file cannot block the
// queue forever.
func (s *Store) DrainOps() (int, error) {
	if s.ReadOnly() {
		return 0, ErrReadOnly
	}
	dir := OpsDir(s.dir)
	names, err := opFiles(dir)
	if err != nil || len(names) == 0 {
		return 0, err
	}
	applied := 0
	for _, name := range names {
		path := filepath.Join(dir, name)
		b, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue // another pass took it
			}
			slog.Warn("mesh: dropping an unreadable op file", "file", name, "err", err)
			_ = os.Remove(path)
			continue
		}
		var op Op
		if err := json.Unmarshal(b, &op); err != nil {
			slog.Warn("mesh: dropping a malformed op file", "file", name, "err", err)
			_ = os.Remove(path)
			continue
		}
		switch op.Kind {
		case OpDeletePending:
			err = s.DeletePending(op.ID)
		case OpRecordWriteback:
			err = s.RecordWriteback(op.NoteID, op.Source)
		case OpTelemetry:
			err = s.applyTelemetryOp(op)
		case OpAddPending:
			if op.Pending == nil {
				slog.Warn("mesh: dropping an add-pending op with no payload", "file", name)
				_ = os.Remove(path)
				continue
			}
			err = s.AddPending(*op.Pending)
		default:
			slog.Warn("mesh: dropping an op of unknown kind", "file", name, "kind", op.Kind)
			_ = os.Remove(path)
			continue
		}
		if err != nil {
			// Leave the file: the effect has NOT landed, and the caller may still be
			// waiting on it. Reporting the error stops the pass from claiming success.
			return applied, fmt.Errorf("applying op %s (%s): %w", name, op.Kind, err)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			// The effect landed but the file survives, so the next pass would replay it.
			// Both ops are idempotent (a DELETE of a gone row, an INSERT ... DO NOTHING),
			// so a replay is harmless; say so rather than failing the drain.
			slog.Warn("mesh: applied an op but could not remove its file; it will replay",
				"file", name, "err", err)
		}
		applied++
	}
	return applied, nil
}

// applyTelemetryOp applies a forwarded batch of usage counters through the exact
// transaction the writable store's own flush uses, so a fetch counts the same whether
// the process that saw it owned the index or not.
func (s *Store) applyTelemetryOp(op Op) error {
	keys := make([]string, 0, len(op.Counts))
	for k := range op.Counts {
		keys = append(keys, k)
	}
	sort.Strings(keys) // stable row order, same as the in-process path
	reuses := make([]reuseEvent, 0, len(op.Reuse))
	for _, r := range op.Reuse {
		reuses = append(reuses, reuseEvent{noteID: r.NoteID, gapSec: r.GapSec, at: r.At})
	}
	if len(keys) == 0 && len(reuses) == 0 {
		return nil
	}
	return s.Write(telemetryTx(keys, op.Counts, reuses))
}

// opFiles lists the queue's op files, oldest first. Temp files (.op-*) are skipped:
// they are in-flight writes, not published ops.
func opFiles(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no queue yet is not an error
		}
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names) // nanosecond-prefixed, so lexical == chronological
	return names, nil
}

func countOps(dir string) (int, error) {
	names, err := opFiles(dir)
	return len(names), err
}

func firstOpErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
