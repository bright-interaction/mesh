// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/index"
)

// The before/after write-lock probe: the measurement this whole refactor is supposed to
// justify, run against both shapes in one test so the comparison is not a memory of an
// earlier run on a different machine.
//
// What it reproduces is the production failure, not a proxy for it. Several `mesh mcp`
// windows sit against one vault; each holds a store and a watcher. The competing writer is
// a plain `index.Open`, which is what every one-shot CLI command does (`mesh index`,
// `mesh code reindex`, `mesh doctor`) and which takes the write lock in ensureSchema. The
// documented symptom is exactly that: "mesh doctor and mesh search fail SQLITE_BUSY at
// apply schema on a current binary while the index is perfectly fresh".
//
// BEFORE is every window writable (the shape before step 1). AFTER is one owning writer
// with read-only windows. The owner is present in BOTH arms, because it always was: the
// contention was never windows-versus-nothing, it was windows versus the owner and each
// other.

const (
	probeVaultNotes  = 250 // enough that a reconcile is a real transaction, not a no-op
	probeWindows     = 5   // concurrent `mesh mcp` windows against one vault
	probeCLIAttempts = 20  // one-shot CLI writers trying to get the lock, as `mesh index` does
	probeWritesPer   = 6   // write-backs each window performs during the run
)

type probeResult struct {
	arm         string
	cliP50      time.Duration
	cliMax      time.Duration
	cliFailures int
	writeP50    time.Duration // agent-visible: how long mesh_append_note took
	writeMax    time.Duration
	walBytes    int64
	elapsed     time.Duration
}

func TestWriteLockProbe(t *testing.T) {
	if testing.Short() {
		t.Skip("measurement pass")
	}
	before := runProbeArm(t, "before: every window writable", true)
	after := runProbeArm(t, "after: one owning writer", false)

	t.Logf("%d windows, %d-note vault, %d write-backs each, %d competing one-shot CLI writers",
		probeWindows, probeVaultNotes, probeWritesPer, probeCLIAttempts)
	for _, r := range []probeResult{before, after} {
		t.Logf("%-30s cli-lock p50=%-8s max=%-8s fail=%d/%d | write-back p50=%-8s max=%-8s | wal=%-7s | %s",
			r.arm, r.cliP50.Round(time.Millisecond), r.cliMax.Round(time.Millisecond),
			r.cliFailures, probeCLIAttempts,
			r.writeP50.Round(time.Millisecond), r.writeMax.Round(time.Millisecond),
			humanBytes(r.walBytes), r.elapsed.Round(time.Millisecond))
	}
	t.Logf("competing writer max wait %s -> %s; WAL %s -> %s",
		before.cliMax.Round(time.Millisecond), after.cliMax.Round(time.Millisecond),
		humanBytes(before.walBytes), humanBytes(after.walBytes))

	// The structural claim, which does not depend on machine speed: the same agent-visible
	// work goes through one writer instead of six, so it WRITES less. WAL bytes is that
	// measurement (nothing checkpoints inside this window, so it is cumulative bytes
	// written), and it is the quantity that reached 223MB in production and starved every
	// other writer into SQLITE_BUSY.
	if after.walBytes >= before.walBytes {
		t.Errorf("the single-writer arm wrote %s against %s for identical work; the point of the split "+
			"is that N windows stop each re-indexing the same vault", humanBytes(after.walBytes), humanBytes(before.walBytes))
	}
	// The accepted tradeoff, stated as a bound rather than left as a surprise: a write-back
	// no longer indexes in-process, so it waits for the owner's debounce and its p50 goes
	// UP. What it buys is a bounded tail. It must stay inside the bound step 2 measured.
	if after.writeMax > OwnerIndexBound {
		t.Errorf("write-back max %s exceeded the bound %s", after.writeMax, OwnerIndexBound)
	}
	// The remaining assertions are absolutes on the AFTER arm only. Asserting that BEFORE
	// is slow would be asserting that a race still loses, which is the kind of test that
	// goes green on a fast machine and gets believed. Those numbers are the artifact.
	if after.cliFailures != 0 {
		t.Errorf("with a single owning writer, %d/%d one-shot CLI writers still could not get the lock",
			after.cliFailures, probeCLIAttempts)
	}
	if after.cliMax > 5*time.Second {
		t.Errorf("a one-shot CLI write waited %s for the lock with only the owner writing; that is the "+
			"contention this split exists to remove", after.cliMax)
	}
	if after.walBytes > 16*1024*1024 {
		t.Errorf("the WAL reached %s with read-only windows; a reader that pins a snapshot stops every "+
			"checkpoint from reclaiming past it, which is how it hit 223MB in production", humanBytes(after.walBytes))
	}
}

func runProbeArm(t *testing.T, arm string, windowsWritable bool) probeResult {
	var res probeResult
	res.arm = arm
	t.Run(arm, func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "decisions"), 0o755); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < probeVaultNotes; i++ {
			body := fmt.Sprintf("---\nid: probe-%03d\ntype: decision\nwhen: 2026-01-01\ndo: a\ndont: b\nwhy: probe note %03d about storage and retrieval\n---\n# Probe %03d\nbody text for the retrieval index\n", i, i, i)
			if err := os.WriteFile(filepath.Join(dir, "decisions", fmt.Sprintf("p%03d.md", i)), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		seedIndex(t, dir)
		// The owning writer, at production cadence, in both arms.
		startOwnerWith(t, dir, 300*time.Millisecond, defaultProbeSweep)

		// The windows. Each runs a watcher, which is what `mesh mcp --watch` does; the tick
		// is compressed so a few seconds of test stands in for a long session.
		windows := make([]*Server, 0, probeWindows)
		for i := 0; i < probeWindows; i++ {
			var srv *Server
			var err error
			if windowsWritable {
				srv, err = NewServerAt(dir, filepath.Join(dir, ".mesh")) // the pre-step-1 shape
			} else {
				srv, err = NewServer(dir)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := srv.WaitReady(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = srv.Watch(ctx, 50*time.Millisecond, 150*time.Millisecond, func(string, ...any) {})
			}()
			t.Cleanup(func() { cancel(); <-done; srv.Close() })
			windows = append(windows, srv)
		}

		start := time.Now()
		var wg sync.WaitGroup
		var mu sync.Mutex
		var writeSamples []time.Duration
		// Every window writes back while the CLI tries to get in. On the writable arm each
		// write-back reindexes in-process (a write transaction); on the read-only arm the
		// same call reaches the index only through the owner.
		for i, srv := range windows {
			wg.Add(1)
			go func(i int, srv *Server) {
				defer wg.Done()
				for j := 0; j < probeWritesPer; j++ {
					t0 := time.Now()
					writeNoteVia(t, srv, fmt.Sprintf("probe window %d write %d", i, j))
					mu.Lock()
					writeSamples = append(writeSamples, time.Since(t0))
					mu.Unlock()
				}
			}(i, srv)
		}

		// The competing one-shot writer: exactly what `mesh index` / `mesh code reindex` do.
		var samples []time.Duration
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < probeCLIAttempts; i++ {
				t0 := time.Now()
				st, err := index.Open(dir)
				if err != nil {
					res.cliFailures++
					samples = append(samples, time.Since(t0))
					continue
				}
				// A real write, not just the open: RecordHealth replaces one issue's rows.
				if err := st.RecordHealth("probe", nil, time.Now()); err != nil {
					res.cliFailures++
				}
				st.Close()
				samples = append(samples, time.Since(t0))
			}
		}()
		wg.Wait()
		res.elapsed = time.Since(start)

		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		if len(samples) > 0 {
			res.cliP50 = samples[len(samples)/2]
			res.cliMax = samples[len(samples)-1]
		}
		sort.Slice(writeSamples, func(i, j int) bool { return writeSamples[i] < writeSamples[j] })
		if len(writeSamples) > 0 {
			res.writeP50 = writeSamples[len(writeSamples)/2]
			res.writeMax = writeSamples[len(writeSamples)-1]
		}
		if fi, err := os.Stat(filepath.Join(dir, ".mesh", "mesh.db-wal")); err == nil {
			res.walBytes = fi.Size()
		}
	})
	return res
}

// defaultProbeSweep matches what an owning writer runs in production (see
// cmd/mesh defaultLocalReconcile), so the arm is not made artificially quiet.
const defaultProbeSweep = 8 * time.Second

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
