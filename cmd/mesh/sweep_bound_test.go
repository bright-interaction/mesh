// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/mcp"
	"github.com/bright-interaction/mesh/internal/watch"
	"github.com/spf13/cobra"
)

// TestLocalSweepStaysUnderTheWriteBackBound pins the one relationship that makes the
// write-back bound honest rather than aspirational.
//
// fsnotify is what normally delivers a note to the owning writer, and it is fast. But two
// measured cases miss it: the moment right after the owner starts, before its watches are
// registered, and a burst caught mid-reconcile. Those notes are picked up only by the
// periodic sweep, and every reader waiting on one gives up at mcp.OwnerIndexBound. With
// the sweep at 30s against a 10s bound, that whole 20s window reported a durable,
// perfectly fine note as owner_down. The two numbers live in different packages, which is
// exactly how they drifted apart, so this asserts the relationship rather than a value.
func TestLocalSweepStaysUnderTheWriteBackBound(t *testing.T) {
	if defaultLocalReconcile >= mcp.OwnerIndexBound {
		t.Fatalf("the owner's periodic sweep is %s but a reader gives up at %s: a note that misses "+
			"its file event reads as owner_down for the difference", defaultLocalReconcile, mcp.OwnerIndexBound)
	}
}

// TestOwningWritersUseTheLocalSweepDefault: the constant above is worth nothing if the
// commands that actually own an index do not use it. All three can be the owning writer
// (`mesh watch`, `mesh mcp --watch`, and `mesh sync --watch`, which is the one running on
// the laptop), so all three carry the same local cadence.
func TestOwningWritersUseTheLocalSweepDefault(t *testing.T) {
	want := defaultLocalReconcile.String()
	for _, tc := range []struct {
		name string
		cmd  func() *cobra.Command
	}{
		{"mesh watch", watchCmd},
		{"mesh mcp", mcpCmd},
		{"mesh sync", syncCmd},
	} {
		f := tc.cmd().Flags().Lookup("reconcile")
		if f == nil {
			t.Fatalf("%s has no --reconcile flag", tc.name)
		}
		if f.DefValue != want {
			t.Errorf("%s --reconcile defaults to %s, want %s (the sweep under the write-back bound)", tc.name, f.DefValue, want)
		}
	}
}

// TestOwningWritersShareTheFullReconcileCadence: the cheap sweep and the authoritative
// content-hash pass are two different cadences on one ticker, and the expensive one is
// what heats an idle laptop. All three owning writers must expose it and default it the
// same way, for the same reason the sweep default is pinned above: `mesh mcp --watch`
// runs once per open agent session, so a command that quietly kept the old
// every-tick-is-authoritative behaviour would put the cost straight back.
func TestOwningWritersShareTheFullReconcileCadence(t *testing.T) {
	want := watch.DefaultFullReconcile.String()
	for _, tc := range []struct {
		name string
		cmd  func() *cobra.Command
	}{
		{"mesh watch", watchCmd},
		{"mesh mcp", mcpCmd},
		{"mesh sync", syncCmd},
	} {
		f := tc.cmd().Flags().Lookup("full-reconcile")
		if f == nil {
			t.Fatalf("%s has no --full-reconcile flag, so its authoritative pass is not tunable", tc.name)
		}
		if f.DefValue != want {
			t.Errorf("%s --full-reconcile defaults to %s, want %s", tc.name, f.DefValue, want)
		}
	}
	if watch.DefaultFullReconcile <= defaultLocalReconcile {
		t.Fatalf("the authoritative pass (%s) is no cheaper than the sweep (%s); the whole point of "+
			"separating them is that parsing every note does not belong on the sweep's cadence",
			watch.DefaultFullReconcile, defaultLocalReconcile)
	}
}

// TestHubRoundIsRateLimitedOnlyOnTheTick is the regression guard for a defect the
// cheap-sweep change introduced and that no existing test could see.
//
// `mesh sync --watch` gated its hub round on the pass being "authoritative", which was a
// stand-in for "this is the periodic tick, so rate-limit it". Making the tick cheap broke
// that stand-in: the tick stopped being authoritative, the gate read every tick as a real
// change, and the daemon went from one hub round a minute to one every 8 seconds, each
// re-hashing the whole vault to build its outbox. The fix reads Pass.Reason; this pins it.
func TestHubRoundIsRateLimitedOnlyOnTheTick(t *testing.T) {
	now := time.Now()
	recent := now.Add(-time.Second)

	for _, tc := range []struct {
		name   string
		reason string
		last   time.Time
		want   bool
	}{
		{"startup always syncs", watch.ReasonStartup, recent, true},
		{"a real change always syncs", watch.ReasonChange, recent, true},
		{"a tick inside the interval does not", watch.ReasonTick, recent, false},
		{"a tick past the interval does", watch.ReasonTick, now.Add(-2 * defaultHubSync), true},
		{"the very first tick does", watch.ReasonTick, time.Time{}, true},
	} {
		if got := hubDue(tc.reason, tc.last, defaultHubSync, now); got != tc.want {
			t.Errorf("%s: hubDue = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestHubSyncKeepsItsOwnCadence: pulling the local sweep under the bound must not drag
// the hub round with it. They answer different questions (has this laptop's own disk
// changed vs has a teammate pushed), and only the first is on the write-back path; SSE
// already delivers the second in real time. Without the split, `mesh sync --watch` would
// have gone from one hub round a minute to seven.
func TestHubSyncKeepsItsOwnCadence(t *testing.T) {
	if defaultHubSync <= defaultLocalReconcile {
		t.Fatalf("the hub round (%s) is now as frequent as the local sweep (%s); the whole point of "+
			"separating them is that a network round trip does not belong on the sweep's cadence",
			defaultHubSync, defaultLocalReconcile)
	}
	f := syncCmd().Flags().Lookup("hub-interval")
	if f == nil {
		t.Fatal("mesh sync has no --hub-interval flag, so the hub cadence is not configurable")
	}
	if f.DefValue != defaultHubSync.String() {
		t.Errorf("mesh sync --hub-interval defaults to %s, want %s", f.DefValue, defaultHubSync)
	}
}
