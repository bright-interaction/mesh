// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWriteContextCancelsWhileWriterIsBusy(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseWriter := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseWriter()
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- s.Write(func(*sql.Tx) error {
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first write did not reach the writer")
	}

	ctx, cancel := context.WithCancel(context.Background())
	var ran atomic.Bool
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- s.WriteContext(ctx, func(*sql.Tx) error {
			ran.Store(true)
			return nil
		})
	}()
	cancel()
	select {
	case err := <-secondDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued write returned %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued write ignored cancellation while the writer was busy")
	}
	if ran.Load() {
		t.Fatal("canceled queued callback ran")
	}

	releaseWriter()
	if err := <-firstDone; err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := s.Write(func(*sql.Tx) error { return nil }); err != nil {
		t.Fatalf("writer did not remain usable: %v", err)
	}
}

func TestWriteContextCancellationRollsBackAcceptedTransaction(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	inserted := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.WriteContext(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `INSERT INTO metrics(key,value) VALUES('context-rollback',1)`); err != nil {
				return err
			}
			close(inserted)
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	select {
	case <-inserted:
	case <-time.After(2 * time.Second):
		t.Fatal("context-bound transaction did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WriteContext returned %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("accepted transaction ignored cancellation")
	}

	// A following write cannot be accepted until the canceled callback has returned,
	// so this is also a deterministic join of the writer's rollback.
	if err := s.Write(func(*sql.Tx) error { return nil }); err != nil {
		t.Fatalf("join writer after rollback: %v", err)
	}
	var n int
	if err := s.readDB.QueryRow(`SELECT count(*) FROM metrics WHERE key='context-rollback'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("canceled transaction committed %d rows", n)
	}
}

func TestWriteContextCancelsContendedBeginBeforeCallback(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Hold SQLite's write reservation from a connection outside Mesh's single-writer
	// queue. modernc's busy handler does not observe context cancellation once entered.
	raw, err := sql.Open("sqlite", dsn(s.dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	conn, err := raw.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("hold external write reservation: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var ran atomic.Bool
	start := time.Now()
	err = s.WriteContext(ctx, func(*sql.Tx) error {
		ran.Store(true)
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended WriteContext returned %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("contended cancellation took %s; the 30s busy handler still won", elapsed)
	}
	if ran.Load() {
		t.Fatal("transaction callback ran without acquiring the contended write lock")
	}

	if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatalf("release external write reservation: %v", err)
	}
	locked = false
	if err := s.Write(func(*sql.Tx) error { return nil }); err != nil {
		t.Fatalf("writer did not recover after canceled contention: %v", err)
	}
}

func TestTelemetryReportFlushIsBoundedByWriteContention(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.IncrMetric("bounded-report", 1)

	raw, err := sql.Open("sqlite", dsn(s.dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	conn, err := raw.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("hold external write reservation: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	start := time.Now()
	if _, err := s.Metric("bounded-report"); err != nil {
		t.Fatalf("read metric after bounded best-effort flush: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("telemetry reporting blocked for %s under contention", elapsed)
	}

	if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatalf("release external write reservation: %v", err)
	}
	locked = false
}

func TestReindexContextPreCanceledLeavesCommittedIndexUntouched(t *testing.T) {
	root := t.TempDir()
	writeContextTestNote(t, root, "old.md", "old")
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := Reindex(s, root); err != nil {
		t.Fatalf("seed index: %v", err)
	}
	writeContextTestNote(t, root, "new.md", "new")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReindexContext(ctx, s, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReindexContext returned %v, want context.Canceled", err)
	}
	if _, err := s.NotePath("new"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("canceled reindex published new note: %v", err)
	}
	if path, err := s.NotePath("old"); err != nil || path != "old.md" {
		t.Fatalf("canceled reindex damaged old snapshot: path=%q err=%v", path, err)
	}
}

func TestReindexContextReturnsCommittedGraphWhenCanceledAfterPersist(t *testing.T) {
	root := t.TempDir()
	writeContextTestNote(t, root, "fresh.md", "fresh")
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	g, err := reindexContext(ctx, s, root, cancel)
	if err != nil {
		t.Fatalf("post-commit cancellation escaped ReindexContext: %v", err)
	}
	if g == nil {
		t.Fatal("ReindexContext returned a nil graph for the committed snapshot")
	}
	if _, found := g.Node("note:fresh"); !found {
		t.Fatal("ReindexContext did not return the graph for the committed snapshot")
	}
	if path, err := s.NotePath("fresh"); err != nil || path != "fresh.md" {
		t.Fatalf("post-commit note was not durable: path=%q err=%v", path, err)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("boundary hook did not cancel context: %v", ctx.Err())
	}
}

func TestContextIndexTailAPIsRejectPreCanceledWork(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	checks := []struct {
		name string
		fn   func() error
	}{
		{"load graph", func() error { _, err := s.LoadGraphContext(ctx); return err }},
		{"id owners", func() error { _, err := s.IDOwnersContext(ctx); return err }},
		{"drain ops", func() error { _, err := s.DrainOpsContext(ctx); return err }},
		{"backfill writebacks", func() error { _, err := s.BackfillWritebacksContext(ctx); return err }},
		{"link notes to code", func() error { _, err := s.LinkNotesToCodeContext(ctx, s.dir); return err }},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.fn(); !errors.Is(err, context.Canceled) {
				t.Fatalf("returned %v, want context.Canceled", err)
			}
		})
	}
}

func TestOpenOwnedContextCancelsSchemaBusyWaitAndReleasesLease(t *testing.T) {
	root := t.TempDir()
	seed, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	meshDir := filepath.Join(root, ".mesh")
	owner, err := AcquireOwnerLock(meshDir, "context-open test", false)
	if err != nil {
		t.Fatal(err)
	}
	ownerReleased := false
	defer func() {
		if !ownerReleased {
			_ = owner.Release()
		}
	}()

	// Hold SQLite's write lock without participating in Mesh ownership. This isolates
	// the schema BEGIN IMMEDIATE wait which used to inherit busy_timeout(30000).
	raw, err := sql.Open("sqlite", dsn(filepath.Join(meshDir, "mesh.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	conn, err := raw.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("hold SQLite write lock: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	store, err := OpenOwnedContext(ctx, root, owner)
	if store != nil {
		_ = store.Close()
		t.Fatal("OpenOwnedContext returned a Store after its schema wait was canceled")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("OpenOwnedContext returned %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("schema cancellation took %s; busy_timeout is still governing startup", elapsed)
	}

	if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatalf("release SQLite write lock: %v", err)
	}
	locked = false

	// The canceled open must release its temporary owner lease. The same declared owner
	// can open immediately, then release its claim for an immediate replacement.
	store, err = OpenOwnedContext(context.Background(), root, owner)
	if err != nil {
		t.Fatalf("open after canceled schema wait: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close successful owned store: %v", err)
	}
	if err := owner.Release(); err != nil {
		t.Fatalf("release owner: %v", err)
	}
	ownerReleased = true
	replacement, err := AcquireOwnerLock(meshDir, "replacement", false)
	if err != nil {
		t.Fatalf("replacement owner after canceled open: %v", err)
	}
	if err := replacement.Release(); err != nil {
		t.Fatalf("release replacement: %v", err)
	}
}

func TestCanceledCombinedIndexWriteKeepsDroppedAndNotesSnapshot(t *testing.T) {
	root := t.TempDir()
	writeContextTestNote(t, root, "old.md", "old")
	if err := os.WriteFile(filepath.Join(root, "broken.md"), []byte("---\nid: [\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := Reindex(s, root); err != nil {
		t.Fatalf("seed index: %v", err)
	}
	if n, err := s.Count("dropped_notes"); err != nil || n != 1 {
		t.Fatalf("seed dropped_notes=%d err=%v, want 1", n, err)
	}

	writeContextTestNote(t, root, "broken.md", "fixed")
	files, err := walkFilesForContextTest(root)
	if err != nil {
		t.Fatal(err)
	}
	notes, _, err := ParseFilesContext(context.Background(), files, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, note := range notes {
		note.Path, _ = filepath.Rel(root, note.Path)
	}
	g, _, err := BuildGraphContext(context.Background(), notes)
	if err != nil {
		t.Fatal(err)
	}
	g.DetectCommunities(0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.indexVaultContext(ctx, notes, g, nil, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("combined index write returned %v, want context.Canceled", err)
	}
	if n, err := s.Count("dropped_notes"); err != nil || n != 1 {
		t.Fatalf("canceled write changed dropped_notes=%d err=%v, want old value 1", n, err)
	}
	if _, err := s.NotePath("fixed"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("canceled write published fixed note: %v", err)
	}
	if path, err := s.NotePath("old"); err != nil || path != "old.md" {
		t.Fatalf("canceled write damaged old notes snapshot: path=%q err=%v", path, err)
	}
}

func writeContextTestNote(t *testing.T, root, name, id string) {
	t.Helper()
	body := []byte("---\nid: " + id + "\ntype: note\n---\n# " + id + "\n")
	if err := os.WriteFile(filepath.Join(root, name), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func walkFilesForContextTest(root string) ([]string, error) {
	return filepath.Glob(filepath.Join(root, "*.md"))
}
