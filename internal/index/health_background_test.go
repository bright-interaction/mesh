// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestSlowBackgroundHealthDoesNotBlockIndexingOrOverlap(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	started, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	var runs atomic.Int32
	s.scheduleBackgroundHealth(dir, func(ctx context.Context) error {
		runs.Add(1)
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	<-started
	live := NewLiveIndexer(s, dir)
	done := make(chan error, 1)
	go func() {
		_, err := live.Reconcile(true)
		if err == nil {
			writeNote(t, dir, "new.md", "---\nid: new\ntype: note\n---\nNew\n")
			_, err = live.ReconcilePaths([]string{"new.md"})
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("indexing waited for the blocked health pass")
	}
	if runs.Load() != 1 {
		t.Fatal("overlapping health workers")
	}
	var count int
	if err := s.readDB.QueryRow(`SELECT count(*) FROM notes WHERE id='new'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("new note not indexed: %d %v", count, err)
	}
}

func TestBackgroundHealthCloseCancelsAndSealsScheduling(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	s.scheduleBackgroundHealth("", func(ctx context.Context) error { close(started); <-ctx.Done(); return ctx.Err() })
	<-started
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not cancel health")
	}
	s.scheduleBackgroundHealth("", func(context.Context) error { t.Error("health restarted after Close"); return nil })
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if !s.healthClosed || s.healthCancel != nil {
		t.Fatal("health lifecycle not sealed")
	}
}

func TestBackgroundHealthRejectsChangedInputsAndKeepsReport(t *testing.T) {
	for _, change := range []string{
		`UPDATE notes SET retrieval_hash='changed' WHERE id='stale'`,
		`UPDATE notes SET frontmatter='{}' WHERE id='stale'`,
		`DELETE FROM notes WHERE id='stale'`,
		`INSERT OR REPLACE INTO meta(key,value) VALUES('code_roots','changed')`,
	} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			note := writeNote(t, dir, "stale.md", "---\nid: stale\ntype: note\nreview_by: 2000-01-01\n---\nOld\n")
			g, _ := BuildGraph([]*ParsedNote{note})
			if _, err := s.IndexVault([]*ParsedNote{note}, g); err != nil {
				t.Fatal(err)
			}
			old := []HealthFinding{{NoteID: "stale", Path: "stale.md", Issue: "overdue", Detail: "previous report"}}
			if err := s.RecordHealth("overdue", old, time.Now()); err != nil {
				t.Fatal(err)
			}
			err = s.refreshBackgroundHealthWith(t.Context(), dir, func() {
				if err := s.Write(func(tx *sql.Tx) error { _, err := tx.Exec(change); return err }); err != nil {
					t.Fatal(err)
				}
			})
			if !errors.Is(err, errHealthChanged) {
				t.Fatalf("wanted changed-input rejection: %v", err)
			}
			got, err := s.ListHealth("overdue")
			if err != nil || len(got) != 1 || got[0].Detail != "previous report" {
				t.Fatalf("replaced old report: %v %v", got, err)
			}
		})
	}
}

func TestBackgroundHealthCancellationAfterAnalysisKeepsReport(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old := []HealthFinding{{NoteID: "old", Issue: "contradiction", Detail: "keep"}}
	if err := s.RecordHealth("contradiction", old, time.Now()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.refreshBackgroundHealthWith(ctx, dir, cancel); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	got, err := s.ListHealth("contradiction")
	if err != nil || len(got) != 1 || got[0].Detail != "keep" {
		t.Fatalf("lost report: %v %v", got, err)
	}
}

func TestBackgroundHealthPublishesAtomicallyAndKeepsOtherIssues(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, issue := range []string{"dead_ref", "overdue", "contradiction", "custom"} {
		if err := s.RecordHealth(issue, []HealthFinding{{NoteID: "old", Issue: issue, Detail: "old"}}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.refreshBackgroundHealth(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	findings, err := s.ListHealth("")
	if err != nil || len(findings) != 1 || findings[0].Issue != "custom" {
		t.Fatalf("bad replacement: %v %v", findings, err)
	}
	var completed string
	if err := s.readDB.QueryRow(`SELECT value FROM meta WHERE key='health_completed_at'`).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339, completed); err != nil {
		t.Fatal(err)
	}
}

func TestBackgroundHealthQueuedPublicationCancels(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	writerDone := make(chan error, 1)
	go func() { writerDone <- s.Write(func(*sql.Tx) error { close(entered); <-release; return nil }) }()
	<-entered
	defer func() {
		close(release)
		if err := <-writerDone; err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := s.refreshBackgroundHealth(ctx, dir); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("queued health write ignored deadline")
	}
}
