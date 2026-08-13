// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
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

// owningWriterConstructors maps a command function DISCOVERED as an owning writer to the
// constructor the flag checks need. It is a lookup, never the subject list: the subjects
// come from the AST (see owningWriterCommands), and a command that claims the vault
// without an entry here fails the build instead of quietly dropping off the guard.
var owningWriterConstructors = map[string]func() *cobra.Command{
	"watchCmd": watchCmd,
	"mcpCmd":   mcpCmd,
	"syncCmd":  syncCmd,
}

// owningWriterCommands returns every function in this package that claims the vault's
// owning-writer role, read out of the AST.
//
// The subject list used to be three names written by hand here, next to a comment
// asserting that `mesh mcp --watch` could be the owning writer. It could not: `mesh mcp`
// opened the index read-only, so its watcher only ever re-read what some other process
// had persisted. The comment was the documentation five other surfaces copied, and both
// tests below stayed green the whole time, because a hand-written list cannot notice that
// one of its entries stopped being what it says it is.
//
// Ownership is now a claim in code (claimIndexOwnership for a declared owner,
// mcp.NewOwningServer for the elected one), so the guard reads the claim instead of a
// human's summary of it, and a fourth owning writer joins these checks by existing.
func owningWriterCommands(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || fn.Body == nil {
					continue
				}
				claims := false
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					switch f := call.Fun.(type) {
					case *ast.Ident:
						if f.Name == "claimIndexOwnership" {
							claims = true
						}
					case *ast.SelectorExpr:
						if f.Sel.Name == "NewOwningServer" {
							claims = true
						}
					}
					return !claims
				})
				if claims {
					out = append(out, fn.Name.Name)
				}
			}
		}
	}
	sort.Strings(out)
	if len(out) < 3 {
		t.Fatalf("found only %d owning-writer command(s) (%v); `mesh watch`, `mesh sync --watch` and "+
			"`mesh mcp` all claim the vault, so the scanner is broken, not the code", len(out), out)
	}
	return out
}

// owningWriterCmd resolves a discovered command name to its constructor, failing when the
// lookup has not kept up with the code.
func owningWriterCmd(t *testing.T, name string) *cobra.Command {
	t.Helper()
	make, ok := owningWriterConstructors[name]
	if !ok {
		t.Fatalf("%s claims the vault's owning-writer role but is not in owningWriterConstructors; "+
			"add it there so the cadence checks below cover it", name)
	}
	return make()
}

// TestOwningWritersUseTheLocalSweepDefault: the constant above is worth nothing if the
// commands that actually own an index do not use it. Every command that claims the vault
// carries the same local cadence, because any of them can be the process a reader is
// waiting on.
func TestOwningWritersUseTheLocalSweepDefault(t *testing.T) {
	want := defaultLocalReconcile.String()
	for _, name := range owningWriterCommands(t) {
		f := owningWriterCmd(t, name).Flags().Lookup("reconcile")
		if f == nil {
			t.Fatalf("%s has no --reconcile flag", name)
		}
		if f.DefValue != want {
			t.Errorf("%s --reconcile defaults to %s, want %s (the sweep under the write-back bound)", name, f.DefValue, want)
		}
	}
}

// TestOwningWritersShareTheFullReconcileCadence: the cheap sweep and the authoritative
// content-hash pass are two different cadences on one ticker, and the expensive one is
// what heats an idle laptop. Every owning writer must expose it and default it the same
// way, for the same reason the sweep default is pinned above: `mesh mcp --watch` runs
// once per open agent session, so a command that quietly kept the old
// every-tick-is-authoritative behaviour would put the cost straight back.
func TestOwningWritersShareTheFullReconcileCadence(t *testing.T) {
	want := watch.DefaultFullReconcile.String()
	for _, name := range owningWriterCommands(t) {
		f := owningWriterCmd(t, name).Flags().Lookup("full-reconcile")
		if f == nil {
			t.Fatalf("%s has no --full-reconcile flag, so its authoritative pass is not tunable", name)
		}
		if f.DefValue != want {
			t.Errorf("%s --full-reconcile defaults to %s, want %s", name, f.DefValue, want)
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
