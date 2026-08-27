// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// The parallel parser must produce byte-identical graph output and identical
// issue ordering no matter how many workers run. This is the guarantee that
// makes the goroutines safe to use.
func TestParseFilesDeterministicAcrossWorkers(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"a.md": "---\nid: a\ntype: note\nwhen: 2026-01-01\n---\n# A\nlinks [[b]] and [[missing]] #x\n",
		"b.md": "---\nid: b\ntype: note\nwhen: 2026-01-01\n---\n# B\nlinks [[c]] #y\n",
		"c.md": "---\nid: c\ntype: note\nwhen: 2026-01-01\n---\n# C\n",
		"d.md": "# D no frontmatter\n[[a]]\n",
	}
	var paths []string
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)

	build := func(workers int) (int, int, []Issue) {
		notes, ferrs := ParseFiles(paths, workers)
		if len(ferrs) != 0 {
			t.Fatalf("workers=%d unexpected parse errors: %v", workers, ferrs)
		}
		g, issues := BuildGraph(notes)
		return g.NodeCount(), g.EdgeCount(), issues
	}

	n1, e1, i1 := build(1)
	n8, e8, i8 := build(8)

	if n1 != n8 || e1 != e8 {
		t.Fatalf("counts differ: workers=1 (%d nodes, %d edges) vs workers=8 (%d, %d)", n1, e1, n8, e8)
	}
	if len(i1) != len(i8) {
		t.Fatalf("issue count differs: %d vs %d", len(i1), len(i8))
	}
	for k := range i1 {
		if i1[k] != i8[k] {
			t.Fatalf("issue order differs at %d: %+v vs %+v", k, i1[k], i8[k])
		}
	}
}

func TestParseFilesContextAlreadyCancelledStartsNoWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	notes, ferrs, err := parseFilesContext(ctx, []string{"a.md"}, 1, func(string) (*ParsedNote, error) {
		called = true
		return &ParsedNote{}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ParseFilesContext error = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("ParseFilesContext invoked the parser with an already-cancelled context")
	}
	if notes != nil || ferrs != nil {
		t.Fatalf("cancelled parse returned partial results: notes=%v errors=%v", notes, ferrs)
	}
}

func TestParseFilesContextCancellationStopsSchedulingAndJoinsWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	paths := []string{"first.md", "second.md", "third.md"}
	started := make(chan string, len(paths))
	finished := make(chan string, len(paths))
	release := make(chan struct{})
	parse := func(path string) (*ParsedNote, error) {
		started <- path
		<-release
		finished <- path
		return &ParsedNote{Path: path}, nil
	}
	type result struct {
		notes []*ParsedNote
		errs  []FileError
		err   error
	}
	done := make(chan result, 1)
	go func() {
		notes, ferrs, err := parseFilesContext(ctx, paths, 1, parse)
		done <- result{notes: notes, errs: ferrs, err: err}
	}()

	select {
	case path := <-started:
		if path != paths[0] {
			t.Fatalf("first scheduled path = %q, want %q", path, paths[0])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first parser did not start")
	}
	cancel()
	select {
	case <-done:
		t.Fatal("ParseFilesContext returned while an already-running parser was still blocked")
	default:
	}
	close(release)

	var got result
	select {
	case got = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ParseFilesContext did not join its worker after cancellation")
	}
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("ParseFilesContext error = %v, want context.Canceled", got.err)
	}
	if got.notes != nil || got.errs != nil {
		t.Fatalf("cancelled parse exposed partial results: notes=%v errors=%v", got.notes, got.errs)
	}
	select {
	case path := <-finished:
		if path != paths[0] {
			t.Fatalf("finished path = %q, want %q", path, paths[0])
		}
	default:
		t.Fatal("ParseFilesContext returned before its active parser finished")
	}
	select {
	case path := <-started:
		t.Fatalf("parser started %q after cancellation", path)
	default:
	}
}

func TestParseFilesContextPreservesInputOrderAcrossOutOfOrderCompletion(t *testing.T) {
	paths := []string{"first.md", "second.md", "third.md"}
	gates := make(map[string]chan struct{}, len(paths))
	for _, path := range paths {
		gates[path] = make(chan struct{})
	}
	started := make(chan string, len(paths))
	finished := make(chan string, len(paths))
	parse := func(path string) (*ParsedNote, error) {
		started <- path
		<-gates[path]
		finished <- path
		return &ParsedNote{Path: path}, nil
	}
	type result struct {
		notes []*ParsedNote
		errs  []FileError
		err   error
	}
	done := make(chan result, 1)
	go func() {
		notes, ferrs, err := parseFilesContext(context.Background(), paths, len(paths), parse)
		done <- result{notes: notes, errs: ferrs, err: err}
	}()

	seen := map[string]bool{}
	for range paths {
		select {
		case path := <-started:
			seen[path] = true
		case <-time.After(2 * time.Second):
			t.Fatal("not every parser started")
		}
	}
	if len(seen) != len(paths) {
		t.Fatalf("started paths = %v, want all %v", seen, paths)
	}
	for i := len(paths) - 1; i >= 0; i-- {
		close(gates[paths[i]])
		select {
		case path := <-finished:
			if path != paths[i] {
				t.Fatalf("completion order = %q, want %q", path, paths[i])
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("parser for %q did not finish", paths[i])
		}
	}

	var got result
	select {
	case got = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ParseFilesContext did not return after every parser finished")
	}
	if got.err != nil || len(got.errs) != 0 {
		t.Fatalf("ParseFilesContext returned errors: parse=%v files=%v", got.err, got.errs)
	}
	if len(got.notes) != len(paths) {
		t.Fatalf("got %d notes, want %d", len(got.notes), len(paths))
	}
	for i, note := range got.notes {
		if note.Path != paths[i] {
			t.Fatalf("note %d path = %q, want input-order %q", i, note.Path, paths[i])
		}
	}
}
