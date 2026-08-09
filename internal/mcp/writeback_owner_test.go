// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// startOwner runs the single owning writer against dir's index for the duration of the
// test: a writable server plus its watcher, which is exactly the production shape
// (`mesh watch` / `mesh sync --watch`, and the hub's own indexing server). The server
// under test opens the SAME index read-only, so these tests exercise the real two-process
// split rather than a stand-in for it.
func startOwner(t *testing.T, dir string) {
	t.Helper()
	startOwnerWith(t, dir, 50*time.Millisecond, 500*time.Millisecond)
}

// startOwnerWith is startOwner with explicit watcher cadence, so a test can run the
// owner at PRODUCTION settings (300ms debounce, 30s periodic reconcile) and tell apart
// "fsnotify delivered the new note" from "the periodic safety net eventually swept it up".
func startOwnerWith(t *testing.T, dir string, debounce, reconcile time.Duration) {
	t.Helper()
	owner, err := NewServerAt(dir, filepath.Join(dir, ".mesh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.WaitReady(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = owner.Watch(ctx, debounce, reconcile, func(string, ...any) {})
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		owner.Close()
	})
	awaitWatcherLive(t, owner, dir)
}

// awaitWatcherLive blocks until the owner's fsnotify watches are actually registered, by
// dropping a probe note and waiting for the owner to index it.
//
// Without this the harness is racy in a way that hides real results: Watch registers its
// watches asynchronously, so a note created in that window generates no event at all and
// waits for the PERIODIC sweep instead. With production's 30s sweep that read as
// "sample 0 never became queryable" and failed only under -v, which is exactly the kind
// of flake that gets rerun until green and believed.
//
// The same window exists in production: a note written in the moment after the owning
// writer starts is missed by fsnotify and picked up only by the sweep. That is survivable
// because the note is durable either way, but it is the reason the periodic sweep matters
// and should not be set far above the write-back bound.
func awaitWatcherLive(t *testing.T, owner *Server, dir string) {
	t.Helper()
	const probeID = "owner-watcher-probe"
	probe := filepath.Join(dir, probeID+".md")
	if err := os.WriteFile(probe,
		[]byte("---\nid: "+probeID+"\ntype: note\nwhen: 2026-01-01\n---\n# Probe\nwatcher liveness probe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	awaitProbe(t, owner, probeID, true, "the owner never indexed the watcher probe, so its watcher never came up")
	// Take the probe out again and wait for THAT to land too, so it leaves no trace in the
	// index. A test that counts what a refresh brought into view (mesh_reindex) would
	// otherwise be measuring the harness: the probe is one more added note, indexed at
	// exactly the moment the test is about to write the note it actually cares about.
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	awaitProbe(t, owner, probeID, false, "the owner never dropped the watcher probe from its index")
}

func awaitProbe(t *testing.T, owner *Server, probeID string, want bool, failure string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, err := owner.store.NotePath(probeID)
		if (err == nil) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(failure)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// writeNoteVia calls mesh_append_note on srv and returns the decoded tool payload.
func writeNoteVia(t *testing.T, srv *Server, title string) map[string]any {
	t.Helper()
	params, err := json.Marshal(map[string]any{
		"name": "mesh_append_note",
		"arguments": map[string]any{
			"type": "gotcha", "title": title,
			"do": "do the thing", "dont": "do not do the other thing", "why": "because",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, rerr := srv.handleToolsCall(WithLocalOperator(context.Background()), params)
	if rerr != nil {
		t.Fatalf("mesh_append_note returned an RPC error, but the note is written to disk before any indexing happens, so a write must never fail here: %+v", rerr)
	}
	return decodeToolPayload(t, res)
}

func decodeToolPayload(t *testing.T, res any) map[string]any {
	t.Helper()
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var wrapper struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(b, &wrapper); err != nil {
		t.Fatal(err)
	}
	if len(wrapper.Content) == 0 {
		t.Fatalf("tool result carried no content: %s", b)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(wrapper.Content[0].Text), &out); err != nil {
		t.Fatalf("tool payload was not JSON: %q", wrapper.Content[0].Text)
	}
	return out
}

// With the owner running, a write-back through a READ-ONLY server becomes queryable
// without that server ever taking the write lock. This is the whole point of the split:
// the note reaches the index through the owner's fsnotify, not through this process.
func TestWriteBackThroughTheOwnerBecomesQueryable(t *testing.T) {
	srv := newTestServer(t)
	startOwner(t, srv.vaultRoot)

	out := writeNoteVia(t, srv, "polling beats a second writer")

	if out["index_stale"] == true {
		t.Fatalf("write-back reported a stale index with the owner running: %v", out)
	}
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("no id in write-back result: %v", out)
	}
	if _, err := srv.store.NotePath(id); err != nil {
		t.Fatalf("note %q is not in the index after a successful write-back: %v", id, err)
	}
}

// The owner-down case the design note insisted on: the note must SAVE durably and fail
// LOUDLY on searchability, never hang forever and never report a failed write (a retry
// mints a duplicate note, which has happened three times in production).
func TestWriteBackWithNoOwnerSavesDurablyAndFailsLoudly(t *testing.T) {
	srv := newTestServer(t)
	srv.ownerIndexTimeout = 300 * time.Millisecond // no owner will ever land it; do not wait 10s

	start := time.Now()
	out := writeNoteVia(t, srv, "the owner is not running")
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("write-back hung for %s with no owner; it must fail on a bound", elapsed)
	}
	// Durable: the file is on disk regardless of any indexing.
	rel, _ := out["path"].(string)
	if rel == "" {
		t.Fatalf("no path in write-back result: %v", out)
	}
	if _, err := os.Stat(filepath.Join(srv.vaultRoot, rel)); err != nil {
		t.Fatalf("the note must be durable on disk even with no owner: %v", err)
	}
	// Loud: the caller is told it is not queryable, and told the real reason.
	if out["index_stale"] != true || out["owner_down"] != true {
		t.Fatalf("a note that never got indexed must report index_stale AND owner_down, got: %v", out)
	}
	warning, _ := out["warning"].(string)
	if warning == "" {
		t.Fatal("owner-down write-back carried no warning")
	}
	// It must NOT send the agent to mesh_reindex: on a read-only server that only
	// re-reads what the owner already persisted, so it cannot fix this.
	if want := "Do NOT call mesh_reindex"; !containsStr(warning, want) {
		t.Fatalf("owner-down warning must steer away from mesh_reindex, got: %q", warning)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestWriteBackLatencyDistribution is the MEASUREMENT the design note required: it
// reports write-to-queryable latency as a distribution against a live owner, so
// ownerIndexTimeout comes from data instead of a guess. It asserts only a generous
// ceiling; the numbers it logs are the artifact.
func TestWriteBackLatencyDistribution(t *testing.T) {
	if testing.Short() {
		t.Skip("measurement pass")
	}
	srv := newTestServer(t)
	// PRODUCTION cadence deliberately. An owner with a fast periodic sweep hides the
	// case that actually sets the bound: under a burst, some notes miss the fsnotify
	// window and wait for the sweep, and in production that sweep is 30s.
	startOwnerWith(t, srv.vaultRoot, 300*time.Millisecond, 30*time.Second)

	const n = 12
	var samples []time.Duration
	for i := 0; i < n; i++ {
		start := time.Now()
		out := writeNoteVia(t, srv, "latency sample "+string(rune('a'+i)))
		d := time.Since(start)
		if out["index_stale"] == true {
			t.Fatalf("sample %d did not become queryable inside the bound: %v", i, out)
		}
		samples = append(samples, d)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	p := func(q float64) time.Duration { return samples[int(float64(len(samples)-1)*q)] }
	t.Logf("write-back to queryable over %d samples: min=%s p50=%s p90=%s max=%s (bound is %s)",
		len(samples), samples[0], p(0.5), p(0.9), samples[len(samples)-1], srv.ownerIndexTimeout)

	if samples[len(samples)-1] > srv.ownerIndexTimeout {
		t.Fatalf("max latency %s exceeded the bound %s", samples[len(samples)-1], srv.ownerIndexTimeout)
	}
}

// The bound is only defensible if fsnotify is what delivers the note. If write-back
// actually rode the owner's PERIODIC reconcile, latency would track that interval, and
// production runs it at 30s, which no sane write-back bound can cover. So: run the owner
// at production cadence with the safety net far out of reach, and require the note to
// land in a small multiple of the debounce. Failing here means the split is relying on
// the sweep rather than on events, and the bound is a fiction.
func TestWriteBackRidesFsnotifyNotThePeriodicSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("timing measurement")
	}
	srv := newTestServer(t)
	const debounce = 300 * time.Millisecond
	startOwnerWith(t, srv.vaultRoot, debounce, 5*time.Minute) // sweep effectively disabled

	start := time.Now()
	out := writeNoteVia(t, srv, "fsnotify must be the trigger")
	elapsed := time.Since(start)

	if out["index_stale"] == true {
		t.Fatalf("note never became queryable with the periodic sweep out of reach, so fsnotify did not deliver it: %v", out)
	}
	t.Logf("write-back to queryable at production cadence (debounce=%s, sweep=5m): %s", debounce, elapsed)
	if elapsed > 10*debounce {
		t.Fatalf("write-back took %s, far past the %s debounce: it is not event-driven", elapsed, debounce)
	}
}
