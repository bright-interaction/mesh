// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// benchVault writes an n-note vault where each note links to two others, giving
// realistic edge density for the graph rebuild.
func benchVault(b *testing.B, n int) string {
	b.Helper()
	dir := b.TempDir()
	for i := 0; i < n; i++ {
		body := fmt.Sprintf("---\nid: n%d\ntype: note\nwhen: 2026-01-01\n---\n# Note %d\nbody text with links [[n%d]] and [[n%d]] #c%d\n",
			i, i, (i+1)%n, (i+2)%n, i%16)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("n%d.md", i)), []byte(body), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	return dir
}

// editNote rewrites note 0 with fresh content so each reconcile sees real drift.
func editNote(b *testing.B, dir string, iter, n int) {
	body := fmt.Sprintf("---\nid: n0\ntype: note\nwhen: 2026-01-01\n---\n# Note 0 rev %d\nedited body [[n%d]] and [[n%d]] #c0\n",
		iter, (iter+1)%n, (iter+2)%n)
	if err := os.WriteFile(filepath.Join(dir, "n0.md"), []byte(body), 0o644); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkReconcileFull measures the OLD watcher cost per single-note edit:
// Reconcile = DriftReport (parse all) + full Reindex (parse all again) + LoadGraph.
func BenchmarkReconcileFull(b *testing.B) {
	for _, n := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("n%d", n), func(b *testing.B) {
			dir := benchVault(b, n)
			s, err := Open(dir)
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			if _, err := Reindex(s, dir); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				editNote(b, dir, i, n)
				b.StartTimer()
				if _, err := Reconcile(s, dir); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkReconcileIncremental measures the NEW watcher cost per single-note edit:
// DriftDeltaReport (parse all once, retain) + in-memory graph rebuild from cache +
// targeted note/FTS writes + nodes/edges rewrite, no LoadGraph, no second parse.
func BenchmarkReconcileIncremental(b *testing.B) {
	for _, n := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("n%d", n), func(b *testing.B) {
			dir := benchVault(b, n)
			s, err := Open(dir)
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			live := NewLiveIndexer(s, dir)
			if _, err := live.Reconcile(true); err != nil { // seed (full)
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				editNote(b, dir, i, n)
				b.StartTimer()
				// authoritative=false: the mtime fast path a debounced change event uses.
				if _, err := live.Reconcile(false); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
