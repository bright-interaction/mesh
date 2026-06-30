// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

// Package watch keeps a Mesh index live with the vault on disk. It is the
// local-first, Obsidian-like immediacy of Milestone S0: edit a note in your
// editor and it is searchable at once, no commit, no manual `mesh index`.
//
// The design mirrors Milestone S's "reconcile-first" principle at local scale.
// The periodic reconcile is the primary consistency mechanism: it runs the same
// authoritative content-hash drift check `mesh doctor` uses, so it always
// converges and a missed or dropped file event never leaves lasting drift. The
// fsnotify event stream is a best-effort speedup layered on top, debounced so a
// burst of editor saves collapses into one reindex. The expensive reindex runs
// single-flight on one goroutine and only rebuilds when retrieval-relevant
// content actually changed, so an idle vault costs nothing but a cheap scan.
package watch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/vault"
	"github.com/fsnotify/fsnotify"
)

// Result is what one reconcile did, for the progress log.
type Result struct {
	Added     int
	Changed   int
	Removed   int
	Reindexed bool
	Dur       time.Duration
}

// Options configure a watch run.
type Options struct {
	Root      string                                   // vault root to watch
	Debounce  time.Duration                            // quiet window to coalesce a save burst; <=0 uses the default
	Reconcile time.Duration                            // periodic safety-net interval; <=0 disables the tick
	OnReindex func(authoritative bool) (Result, error) // drift-check + reindex, single-flight; authoritative=false on a debounced change (mtime fast path ok), true on startup + the periodic safety tick (full hash check)
	Logf      func(string, ...any)                     // progress sink; nil is silent
	Trigger   <-chan struct{}                          // optional external nudge (e.g. an SSE event); fires OnReindex like a local change
}

const defaultDebounce = 300 * time.Millisecond

// Run watches opt.Root until ctx is cancelled, calling opt.OnReindex whenever
// the vault changes (debounced) and on a periodic safety tick. It reconciles
// once at startup so the index reflects disk from the first moment. Run blocks;
// callers typically run it in a goroutine alongside their main loop.
func Run(ctx context.Context, opt Options) error {
	if opt.OnReindex == nil {
		return errors.New("watch: OnReindex is required")
	}
	if opt.Debounce <= 0 {
		opt.Debounce = defaultDebounce
	}
	logf := opt.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	if err := addWatches(w, opt.Root, logf); err != nil {
		return err
	}

	// Reflect disk from the moment we start (bootstraps an empty index too).
	reconcile(opt, logf, "startup")

	// Debounce timer, created stopped: armed only once an event arrives.
	debounce := time.NewTimer(opt.Debounce)
	if !debounce.Stop() {
		<-debounce.C
	}
	defer debounce.Stop()

	var tick <-chan time.Time
	if opt.Reconcile > 0 {
		t := time.NewTicker(opt.Reconcile)
		defer t.Stop()
		tick = t.C
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			switch {
			case ev.Op&fsnotify.Create != 0 && isDir(ev.Name):
				// A new subdirectory: kqueue/inotify watch one dir at a time, so
				// add it (and any children) to the watch set, then reconcile in
				// case files landed inside before the watch was in place.
				_ = addWatches(w, opt.Root, logf)
				resetTimer(debounce, opt.Debounce)
			case ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 && watched(w, ev.Name):
				// A watched directory went away: drop its now-dangling watch so we
				// do not leak a descriptor, and reconcile so its notes leave the
				// index promptly rather than waiting for the periodic tick.
				_ = w.Remove(ev.Name)
				resetTimer(debounce, opt.Debounce)
			case relevant(ev):
				resetTimer(debounce, opt.Debounce)
			}
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			logf("watch error: %v", err)
		case <-opt.Trigger:
			// An external nudge (a hub SSE "head changed" event). Treat it like a
			// local change: arm the debounce so a burst of nudges coalesces into one
			// reconcile. A nil Trigger channel blocks forever, so this case is inert
			// unless a caller wired one in.
			resetTimer(debounce, opt.Debounce)
		case <-debounce.C:
			reconcile(opt, logf, "change")
		case <-tick:
			// Safety net: the authoritative content-hash reconcile catches
			// anything the event stream missed (a dropped event, a same-second
			// rename). With no real edits it is a fast drift check that finds
			// nothing and rebuilds nothing.
			reconcile(opt, logf, "reconcile")
		}
	}
}

func reconcile(opt Options, logf func(string, ...any), reason string) {
	// A debounced local change can use the mtime fast path; startup and the periodic
	// safety tick run an authoritative full hash check so a mtime-preserving edit (or
	// any missed event) is always caught.
	authoritative := reason != "change"
	res, err := opt.OnReindex(authoritative)
	if err != nil {
		logf("reindex failed (%s): %v", reason, err)
		return
	}
	if res.Reindexed {
		logf("reindexed +%d ~%d -%d in %s (%s)",
			res.Added, res.Changed, res.Removed, res.Dur.Round(time.Millisecond), reason)
	}
}

// addWatches (re)adds a watch on every indexed directory under root. fsnotify
// dedupes repeat adds, so calling it again after a new directory appears simply
// picks up the newcomer. It honors the same skip rules the indexer walks with.
func addWatches(w *fsnotify.Watcher, root string, logf func(string, ...any)) error {
	dirs, err := vault.Dirs(root)
	if err != nil {
		return err
	}
	for _, d := range dirs {
		if err := w.Add(d); err != nil {
			logf("watch add %s: %v", d, err)
		}
	}
	return nil
}

// watched reports whether path is currently in the fsnotify watch set (i.e. a
// directory we added), so a Remove/Rename event can be told apart from churn on
// files and temp artifacts we never watched.
func watched(w *fsnotify.Watcher, path string) bool {
	clean := filepath.Clean(path)
	for _, p := range w.WatchList() {
		if filepath.Clean(p) == clean {
			return true
		}
	}
	return false
}

// relevant reports whether an event is a content change to a markdown note.
// Pure Chmod events (and non-.md files) are ignored.
func relevant(ev fsnotify.Event) bool {
	if !strings.EqualFold(filepath.Ext(ev.Name), ".md") {
		return false
	}
	if vault.IsConflictSibling(filepath.Base(ev.Name)) {
		return false // conflict artifacts are not indexed; ignore their churn
	}
	return ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) != 0
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// resetTimer safely restarts a timer for debouncing: stop, drain any pending
// fire, then reset. Called only from Run's single goroutine.
func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}
