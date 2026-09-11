// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/graph"
)

type writerLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *writerLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *writerLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func captureWriterTiming(t *testing.T) *writerLogBuffer {
	t.Helper()
	output := &writerLogBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return output
}

func TestWriterTimingSeparatesCanceledQueueFromTransaction(t *testing.T) {
	output := captureWriterTiming(t)
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.Write(func(tx *sql.Tx) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 1100*time.Millisecond)
	defer cancel()
	called := false
	err = s.WriteContext(ctx, func(*sql.Tx) error {
		called = true
		return nil
	})
	close(release)
	if firstErr := <-done; firstErr != nil {
		t.Fatal(firstErr)
	}
	if !errors.Is(err, context.DeadlineExceeded) || called {
		t.Fatalf("queued cancellation changed: callback=%t err=%v", called, err)
	}
	got := output.String()
	for _, want := range []string{"operation=index_write", "last_phase=queue", "operation=index_transaction", "phases.callback=", "phases.commit="} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if strings.Contains(got, s.dbPath) {
		t.Fatal("writer timing leaked the database path")
	}
}

type slowPrivateAttrs struct{}

func (slowPrivateAttrs) MarshalJSON() ([]byte, error) {
	time.Sleep(1100 * time.Millisecond)
	return []byte(`"private-body-value"`), nil
}

func TestWriterTimingLocatesGraphEncodingWithoutContent(t *testing.T) {
	output := captureWriterTiming(t)
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	g := graph.New()
	g.AddNode(&graph.Node{ID: "private-note-id", Kind: "note", Label: "private-title", Attrs: map[string]any{"secret": slowPrivateAttrs{}}})
	if _, err := s.IndexVaultIncremental(nil, nil, g); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{"operation=persist_graph_delta", "phases.encode=", "operation=persist_incremental", "phases.graph=", "operation=index_transaction", "phases.callback="} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	for _, secret := range []string{s.dbPath, "private-note-id", "private-title", "private-body-value"} {
		if strings.Contains(got, secret) {
			t.Fatal("persistence timing leaked user data")
		}
	}
}

func TestWriterTimingReportsRollbackWithoutRawError(t *testing.T) {
	output := captureWriterTiming(t)
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sentinel := errors.New("private-sql-error")
	err = s.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('timing-rollback','private-value')`); err != nil {
			return err
		}
		time.Sleep(1100 * time.Millisecond)
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	var count int
	if err := s.readDB.QueryRow(`SELECT count(*) FROM meta WHERE key='timing-rollback'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback changed: count=%d err=%v", count, err)
	}
	got := output.String()
	if !strings.Contains(got, "phases.rollback=") || strings.Contains(got, "private-") {
		t.Fatalf("rollback timing missing or leaked error: %s", got)
	}
}
