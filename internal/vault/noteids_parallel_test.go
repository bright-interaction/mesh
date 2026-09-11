// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func idTestWalker(names []string) func(string, fs.WalkDirFunc) error {
	return func(root string, visit fs.WalkDirFunc) error {
		for _, name := range names {
			if err := visit(filepath.Join(root, name), staticDirEntry{name: name}, nil); err != nil {
				return err
			}
		}
		return nil
	}
}

func TestParallelIDScanKeepsTraversalOrder(t *testing.T) {
	names := []string{"a.md", "b.md", "c.md", "d.md"}
	ready := make(chan string, len(names))
	gates := map[string]chan struct{}{}
	finished := map[string]chan struct{}{}
	for _, name := range names {
		gates[name], finished[name] = make(chan struct{}), make(chan struct{})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan idHoldersResult, 1)
	go func() {
		holders, err := scanClaimedIDHolders(ctx, "/vault", idTestWalker(names),
			func(ctx context.Context, path string) (string, bool, error) {
				name := filepath.Base(path)
				ready <- name
				select {
				case <-ctx.Done():
					return "", false, ctx.Err()
				case <-gates[name]:
				}
				close(finished[name])
				return "shared", name != "a.md", nil
			})
		done <- idHoldersResult{holders, err}
	}()
	for range names {
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatal("readers did not overlap")
		}
	}
	// Reverse completion order. b must still beat c/d; a's filename fallback
	// must yield to the first declared id, exactly as in the serial scan.
	for i := len(names) - 1; i >= 0; i-- {
		close(gates[names[i]])
		select {
		case <-finished[names[i]]:
		case <-ctx.Done():
			t.Fatal("reader did not finish")
		}
	}
	got := <-done
	if got.err != nil || got.holders["shared"] != (idHolder{path: "b.md", declared: true}) {
		t.Fatalf("completion order changed incumbent: %+v", got)
	}
}

func TestParallelIDScanBoundsReadersAndDiscardsCancellation(t *testing.T) {
	names := make([]string, 40)
	for i := range names {
		names[i] = fmt.Sprintf("%02d.md", i)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	started := make(chan struct{}, len(names))
	done := make(chan idHoldersResult, 1)
	go func() {
		holders, err := scanClaimedIDHolders(ctx, "/vault", idTestWalker(names),
			func(ctx context.Context, _ string) (string, bool, error) {
				calls.Add(1)
				started <- struct{}{}
				<-ctx.Done()
				return "", false, ctx.Err()
			})
		done <- idHoldersResult{holders, err}
	}()
	for range idScanWorkers {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("worker pool did not start")
		}
	}
	cancel()
	got := <-done
	if calls.Load() != idScanWorkers || got.holders != nil || !errors.Is(got.err, context.Canceled) {
		t.Fatalf("unbounded or partial canceled scan: calls=%d result=%+v", calls.Load(), got)
	}
}

func TestParallelIDScanMatchesSerialOnDiskChanges(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"a.md":      "# fallback",
		"b.md":      "---\nid: a\n---\n# declared beats fallback",
		"c.md":      "---\nid: a\n---\n# later declared loses",
		"custom.md": "---\nid: old\n---\n",
		"broken.md": "---\nid: [\n---\n",
		"blank.MD":  "plain text",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	check := func() map[string]idHolder {
		t.Helper()
		want, err := scanClaimedIDHoldersWorkers(context.Background(), root, filepath.WalkDir, noteIDForFileContext, 1)
		if err != nil {
			t.Fatal(err)
		}
		got, err := claimedIDHoldersContext(context.Background(), root)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("parallel/serial mismatch: got=%v want=%v err=%v", got, want, err)
		}
		return got
	}
	check()
	path := filepath.Join(root, "custom.md")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Same size and restored mtime: a cache keyed only by metadata would miss it.
	if err := os.WriteFile(path, []byte("---\nid: new\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	got := check()
	if _, old := got["old"]; old || got["new"].path != "custom.md" {
		t.Fatalf("fresh-disk collision scan lost metadata-preserving edit: %v", got)
	}
}

func TestParallelIDScanPreservesPartialWalkError(t *testing.T) {
	sentinel := errors.New("walk stopped")
	walk := func(root string, visit fs.WalkDirFunc) error {
		if err := idTestWalker([]string{"a.md", "b.md"})(root, visit); err != nil {
			return err
		}
		return sentinel
	}
	got, err := scanClaimedIDHolders(context.Background(), "/vault", walk,
		func(_ context.Context, path string) (string, bool, error) {
			return filepath.Base(path), true, nil
		})
	if !errors.Is(err, sentinel) || len(got) != 2 {
		t.Fatalf("ordinary walk error lost historical partial-map behavior: %v %v", got, err)
	}
}

func BenchmarkClaimedIDScan(b *testing.B) {
	root := b.TempDir()
	for i := 0; i < 3000; i++ {
		body := fmt.Sprintf("---\nid: note-%d\ntype: decision\ndo: preserve collisions\ndont: trust stale metadata\nwhy: efficient shared knowledge\n---\n# Note\n%s", i, strings.Repeat("body ", 400))
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("%04d.md", i)), []byte(body), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name    string
		workers int
		read    func(context.Context, string) (string, bool, error)
	}{
		{"legacy-serial", 1, legacyIDHeadRead},
		{"serial-growing", 1, noteIDForFileContext},
		{"parallel-growing", idScanWorkers, noteIDForFileContext},
	} {
		b.Run(tc.name, func(b *testing.B) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				got, err := scanClaimedIDHoldersWorkers(ctx, root, filepath.WalkDir, tc.read, tc.workers)
				if err != nil || len(got) != 3000 {
					b.Fatalf("scan: %d claims, %v", len(got), err)
				}
			}
		})
	}
}

// Retain the pre-v0.25 allocation/read shape as a reproducible benchmark baseline.
// It shares the unchanged YAML decoder; benchmark files are valid regular notes.
func legacyIDHeadRead(ctx context.Context, path string) (string, bool, error) {
	head, err := readBytesContext(ctx, func() ([]byte, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		head := make([]byte, idScanBytes)
		n, err := io.ReadFull(f, head)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			err = nil
		}
		return head[:n], err
	})
	if err != nil {
		return "", false, err
	}
	key := strings.ToLower(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	id, declared := noteIDFromHead(key, head)
	return id, declared, nil
}

func TestGrowingHeadMatchesLegacyBoundaries(t *testing.T) {
	root := t.TempDir()
	for _, size := range []int{0, 4095, 4096, 4097, idScanBytes - 4, idScanBytes, idScanBytes + 4} {
		// Move the closing marker across each growth boundary and the byte cap.
		content := "---\nid: declared\n# " + strings.Repeat("x", size) + "\n---\nbody"
		path := filepath.Join(root, fmt.Sprintf("n%d.md", size))
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		want, wd, err := legacyIDHeadRead(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		got, gd, err := noteIDForFileContext(context.Background(), path)
		if err != nil || got != want || gd != wd {
			t.Fatalf("size %d: got %q/%t/%v want %q/%t", size, got, gd, err, want, wd)
		}
	}
}
