// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
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

	"github.com/bright-interaction/mesh/internal/vault"
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
// cannot be applied before the note it refers to was created. Durable bookkeeping ops
// are removed only after their effect commits. Telemetry is the deliberate exception:
// additive counters are best-effort and non-idempotent, so their file is unlinked and
// the directory fsynced BEFORE apply. A crash may lose that batch, but can never replay
// it and double-count it.

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
	_ = syncDir(dir)
	return name, nil
}

// syncDir fsyncs a directory so a rename/unlink in it survives a power loss. Enqueue is
// deliberately best-effort when a filesystem refuses directory sync; the telemetry
// at-most-once claim is not, because applying after a failed unlink sync could replay an
// additive increment after reboot.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	return errors.Join(syncErr, closeErr)
}

// OpQueued reports whether a queued op file is still waiting to be consumed. This is the
// readiness signal a read-only bookkeeping caller polls, the exact counterpart of
// polling NotePath for a written note. Durable bookkeeping files disappear after commit;
// best-effort telemetry disappears when it is claimed, before its additive transaction.
func (s *Store) OpQueued(name string) bool {
	if name == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(OpsDir(s.dir), name))
	return err == nil
}

// DrainOps consumes every queued op, oldest first. Only the owning writer may call it;
// a read-only store refuses, because silently doing nothing here would look exactly like
// a healthy drain.
//
// A durable bookkeeping op whose apply FAILS is left in place for the next pass (a
// transient DB error must not lose a promoted note's bookkeeping). Telemetry is claimed
// by unlinking before apply because its increments are non-idempotent and explicitly
// best-effort. An op that can never succeed (unreadable, malformed, unknown kind) is
// removed and logged, so one bad file cannot block the queue forever.
func (s *Store) DrainOps() (int, error) {
	return s.DrainOpsContext(context.Background())
}

// DrainOpsContext is DrainOps with cooperative cancellation between op files and a
// context-bound transaction for each applied effect. Interrupted durable bookkeeping
// stays on disk; telemetry already claimed at-most-once may be dropped.
func (s *Store) DrainOpsContext(ctx context.Context) (int, error) {
	return s.drainOpsContext(ctx, opFilesContext, vault.ReadFileContext, os.Remove)
}

type listOpFilesFunc func(context.Context, string) ([]string, error)
type readOpFileFunc func(context.Context, string) ([]byte, error)

// drainOpsContext keeps the filesystem boundaries injectable so cancellation can be
// proved without relying on a particular FUSE/network filesystem. Directory listing
// and file reads may finish late only in read-only workers; removal and every database
// mutation remain synchronous and are never started after cancellation.
func (s *Store) drainOpsContext(ctx context.Context, listFiles listOpFilesFunc, readFile readOpFileFunc, removeFile func(string) error) (int, error) {
	return s.drainOpsContextWithTelemetry(ctx, listFiles, readFile, removeFile, s.applyTelemetryOpContext)
}

type applyTelemetryOpFunc func(context.Context, Op) error

func (s *Store) drainOpsContextWithTelemetry(ctx context.Context, listFiles listOpFilesFunc, readFile readOpFileFunc, removeFile func(string) error, applyTelemetry applyTelemetryOpFunc) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if s.ReadOnly() {
		return 0, ErrReadOnly
	}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-s.opsDrainGate:
	}
	defer func() { s.opsDrainGate <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	dir := OpsDir(s.dir)
	names, err := listFiles(ctx, dir)
	if err != nil || len(names) == 0 {
		return 0, err
	}
	applied := 0
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return applied, err
		}
		path := filepath.Join(dir, name)
		b, err := readFile(ctx, path)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return applied, ctxErr
			}
			if os.IsNotExist(err) {
				continue // another pass took it
			}
			slog.Warn("mesh: dropping an unreadable op file", "file", name, "err", err)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return applied, ctxErr
			}
			_ = removeFile(path)
			continue
		}
		var op Op
		if err := json.Unmarshal(b, &op); err != nil {
			slog.Warn("mesh: dropping a malformed op file", "file", name, "err", err)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return applied, ctxErr
			}
			_ = removeFile(path)
			continue
		}
		telemetryClaimed := false
		if op.Kind == OpTelemetry {
			// Telemetry applies additive increments, so commit-then-unlink can replay a
			// committed batch after an unlink failure or crash. Claim it at-most-once by
			// durably removing the queue entry first. If another drainer won the unlink,
			// it alone owns the batch; this drainer must never apply it.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return applied, ctxErr
			}
			if err := removeFile(path); err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return applied, fmt.Errorf("claiming telemetry op %s: %w", name, err)
			}
			if err := syncDir(dir); err != nil {
				// The file is already unlinked, so this best-effort batch is lost. Do not
				// apply without proving that unlink durable: a reboot could resurrect the
				// file beside a surviving SQLite increment and double-count it.
				return applied, fmt.Errorf("sync claimed telemetry op %s: %w", name, err)
			}
			telemetryClaimed = true
			if ctxErr := ctx.Err(); ctxErr != nil {
				return applied, ctxErr
			}
		}
		switch op.Kind {
		case OpDeletePending:
			err = s.DeletePendingContext(ctx, op.ID)
		case OpRecordWriteback:
			err = s.RecordWritebackContext(ctx, op.NoteID, op.Source)
		case OpTelemetry:
			err = applyTelemetry(ctx, op)
		case OpAddPending:
			if op.Pending == nil {
				slog.Warn("mesh: dropping an add-pending op with no payload", "file", name)
				if ctxErr := ctx.Err(); ctxErr != nil {
					return applied, ctxErr
				}
				_ = removeFile(path)
				continue
			}
			err = s.AddPendingContext(ctx, *op.Pending)
		default:
			slog.Warn("mesh: dropping an op of unknown kind", "file", name, "kind", op.Kind)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return applied, ctxErr
			}
			_ = removeFile(path)
			continue
		}
		if err != nil {
			// Durable bookkeeping stays queued because its effect has not landed. A
			// telemetry file was already durably claimed: losing a best-effort batch is
			// preferable to replaying an ambiguous additive transaction.
			return applied, fmt.Errorf("applying op %s (%s): %w", name, op.Kind, err)
		}
		if telemetryClaimed {
			applied++
			continue
		}
		// The transaction above may have committed just as shutdown arrived. Leave the
		// remaining idempotent bookkeeping op in place for the replacement owner rather
		// than beginning a new filesystem mutation after cancellation.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return applied, ctxErr
		}
		if err := removeFile(path); err != nil && !os.IsNotExist(err) {
			// The effect landed but the file survives, so the next pass will replay it.
			// Every kind that reaches this path is idempotent (DELETE, INSERT ... DO
			// NOTHING, or a stable-id upsert), so replay is harmless.
			slog.Warn("mesh: applied an op but could not remove its file; it will replay",
				"file", name, "err", err)
		}
		applied++
	}
	return applied, nil
}

func newOpsDrainGate() chan struct{} {
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return gate
}

// applyTelemetryOp applies a forwarded batch of usage counters through the exact
// transaction the writable store's own flush uses, so a fetch counts the same whether
// the process that saw it owned the index or not.
func (s *Store) applyTelemetryOp(op Op) error {
	return s.applyTelemetryOpContext(context.Background(), op)
}

func (s *Store) applyTelemetryOpContext(ctx context.Context, op Op) error {
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
	return s.WriteContext(ctx, telemetryTxContext(ctx, keys, op.Counts, reuses))
}

// opFiles lists the queue's op files, oldest first. Temp files (.op-*) are skipped:
// they are in-flight writes, not published ops.
func opFiles(dir string) ([]string, error) {
	return opFilesContext(context.Background(), dir)
}

type readDirResult struct {
	entries []os.DirEntry
	err     error
}

// opFilesContext is opFiles with a caller-owned wait around os.ReadDir. ReadDir itself
// has no context and can block on an unhealthy mount, so a cancellable caller runs it in
// a read-only worker whose only publication is this private buffered result.
func opFilesContext(ctx context.Context, dir string) ([]string, error) {
	return opFilesContextWith(ctx, dir, os.ReadDir)
}

func opFilesContextWith(ctx context.Context, dir string, readDir func(string) ([]os.DirEntry, error)) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var ents []os.DirEntry
	var err error
	if ctx.Done() == nil {
		ents, err = readDir(dir)
	} else {
		result := make(chan readDirResult, 1)
		go func() {
			entries, readErr := readDir(dir)
			result <- readDirResult{entries: entries, err: readErr}
		}()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case got := <-result:
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			ents, err = got.entries, got.err
		}
	}
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no queue yet is not an error
		}
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names) // nanosecond-prefixed, so lexical == chronological
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
