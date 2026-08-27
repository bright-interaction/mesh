//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

type fifoOpenResult struct {
	file *os.File
	err  error
}

// openFIFOTestWriter starts a writer whose successful open is also the deterministic
// proof that the production reader has opened the other end and is stalled waiting for
// bytes. The caller must close the returned file to let the isolated read finish.
func openFIFOTestWriter(path string) <-chan fifoOpenResult {
	ready := make(chan fifoOpenResult, 1)
	go func() {
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		ready <- fifoOpenResult{file: f, err: err}
	}()
	return ready
}

func waitFIFOTestWriter(t *testing.T, ready <-chan fifoOpenResult) *os.File {
	t.Helper()
	select {
	case opened := <-ready:
		if opened.err != nil {
			t.Fatalf("open FIFO writer: %v", opened.err)
		}
		return opened.file
	case <-time.After(2 * time.Second):
		t.Fatal("filesystem reader did not reach the FIFO")
		return nil
	}
}

func waitFIFOTestReaderClosed(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
		switch {
		case err == syscall.ENXIO:
			return // no reader remains
		case err != nil:
			t.Fatalf("probe FIFO reader: %v", err)
		default:
			_ = syscall.Close(fd)
			time.Sleep(time.Millisecond)
		}
	}
	t.Fatal("isolated FIFO reader did not exit after the writer closed")
}

func TestParseFilesContextCancelsWhileProductionReadIsStalled(t *testing.T) {
	root := t.TempDir()
	regular := filepath.Join(root, "first.md")
	if err := os.WriteFile(regular, []byte("---\nid: first\ntype: note\n---\n# First\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(root, "blocked.md")
	if err := syscall.Mkfifo(blocked, 0o600); err != nil {
		t.Fatal(err)
	}

	writerReady := openFIFOTestWriter(blocked)
	ctx, cancel := context.WithCancel(context.Background())
	type parseResult struct {
		notes []*ParsedNote
		ferrs []FileError
		err   error
	}
	done := make(chan parseResult, 1)
	go func() {
		notes, ferrs, err := ParseFilesContext(ctx, []string{regular, blocked}, 1)
		done <- parseResult{notes: notes, ferrs: ferrs, err: err}
	}()

	writer := waitFIFOTestWriter(t, writerReady)
	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("ParseFilesContext returned %v, want context.Canceled", got.err)
		}
		if got.notes != nil || got.ferrs != nil {
			t.Fatalf("cancelled parse exposed partial results: notes=%v errors=%v", got.notes, got.ferrs)
		}
	case <-time.After(2 * time.Second):
		writer.Close()
		t.Fatal("ParseFilesContext waited for the stalled file read after cancellation")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	waitFIFOTestReaderClosed(t, blocked)
}

func TestLinkNotesToCodeContextCancelsStalledReadWithoutReplacingLinks(t *testing.T) {
	vaultRoot := t.TempDir()
	notePath := filepath.Join(vaultRoot, "note.md")
	note := "---\nid: note-x\ntype: note\n---\n# Link\nUses `DistinctiveSymbol`.\n"
	if err := os.WriteFile(notePath, []byte(note), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := Open(vaultRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := Reindex(store, vaultRoot); err != nil {
		t.Fatal(err)
	}

	codeRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(codeRoot, "symbol.go"), []byte("package code\nfunc DistinctiveSymbol() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReindexCode(store, []string{codeRoot}, nil); err != nil {
		t.Fatal(err)
	}
	if n, err := store.LinkNotesToCode(vaultRoot); err != nil || n != 1 {
		t.Fatalf("seed links: count=%d err=%v", n, err)
	}

	if err := os.Remove(notePath); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(notePath, 0o600); err != nil {
		t.Fatal(err)
	}
	writerReady := openFIFOTestWriter(notePath)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := store.LinkNotesToCodeContext(ctx, vaultRoot)
		done <- err
	}()

	writer := waitFIFOTestWriter(t, writerReady)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("LinkNotesToCodeContext returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		writer.Close()
		t.Fatal("LinkNotesToCodeContext waited for the stalled file read after cancellation")
	}

	// The replacement transaction must never have started: the last complete bridge
	// remains visible even while the abandoned filesystem read is still blocked.
	syms, err := store.SymbolsForNote("note-x")
	if err != nil {
		t.Fatal(err)
	}
	if len(syms) != 1 || syms[0].Symbol != "DistinctiveSymbol" {
		t.Fatalf("cancelled rebuild replaced the committed bridge: %+v", syms)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	waitFIFOTestReaderClosed(t, notePath)
	// Once the late read has definitely exited, there is still no continuation that
	// can publish its result or start a replacement transaction.
	syms, err = store.SymbolsForNote("note-x")
	if err != nil {
		t.Fatal(err)
	}
	if len(syms) != 1 || syms[0].Symbol != "DistinctiveSymbol" {
		t.Fatalf("late read completion replaced the committed bridge: %+v", syms)
	}
}
