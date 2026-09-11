// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/watch"
	"github.com/bright-interaction/mesh/pkg/meshclient"
)

func waitSyncWatchSignal(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout: " + label)
	}
}

// Replaces the old synchronous ordering tests with the actual deployed runner:
// notes arriving AFTER sync began must commit before that network call returns.
func TestSyncWatchIndexesBeforeAndDuringBlockedHub(t *testing.T) {
	root := t.TempDir()
	writeNote := func(id string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, id+".md"), []byte("---\nid: "+id+"\ntype: note\n---\nLocal content.\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeNote("initial")
	store, err := index.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	live := index.NewLiveIndexer(store, root)
	started, release, failed := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	var releaseOnce sync.Once
	var calls, active, maxActive atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runSyncWatch(ctx, watch.Options{Root: root, Debounce: 20 * time.Millisecond, Reconcile: 40 * time.Millisecond,
			Logf: func(f string, a ...any) {
				if strings.Contains(fmt.Sprintf(f, a...), "file name too long") {
					select {
					case failed <- struct{}{}:
					default:
					}
				}
			},
			OnReindex: func(p watch.Pass) (watch.Result, error) {
				if len(p.Paths) > 0 {
					_, err := live.ReconcilePaths(p.Paths)
					return watch.Result{}, err
				}
				_, err := live.Reconcile(p.Authoritative)
				return watch.Result{}, err
			}}, time.Hour, func() (meshclient.Summary, error) {
			n := active.Add(1)
			defer active.Add(-1)
			if n > maxActive.Load() {
				maxActive.Store(n)
			}
			if calls.Add(1) == 1 {
				if _, err := store.NotePath("initial"); err != nil {
					t.Error("hub started before local publication")
				}
				close(started)
				<-release
				return meshclient.Summary{}, syscall.ENAMETOOLONG
			}
			return meshclient.Summary{Head: "head"}, nil
		})
	}()
	t.Cleanup(func() {
		cancel()
		releaseOnce.Do(func() { close(release) })
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	waitSyncWatchSignal(t, started, "first hub round")
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("during-%d", i)
		writeNote(id)
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := store.NotePath(id); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("local note waited behind blocked hub")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("overlapping hub rounds")
	}
	releaseOnce.Do(func() { close(release) })
	waitSyncWatchSignal(t, failed, "reported hub failure")
	deadline := time.Now().Add(10 * time.Second)
	for calls.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("edits during sync lost their follow-up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if maxActive.Load() != 1 {
		t.Fatal("sync worker not serial")
	}
}

func TestSyncWatchCompletionDoesNotResyncAndShutdownDrains(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(fmt.Sprint(blocked), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			started, release, refreshed := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
			var once sync.Once
			var calls atomic.Int32
			done := make(chan error, 1)
			go func() {
				done <- runSyncWatch(ctx, watch.Options{Root: t.TempDir(), Debounce: 10 * time.Millisecond,
					OnReindex: func(p watch.Pass) (watch.Result, error) {
						if p.Reason == watch.ReasonRefresh {
							select {
							case refreshed <- struct{}{}:
							default:
							}
						}
						return watch.Result{}, nil
					}}, time.Hour,
					func() (meshclient.Summary, error) {
						if calls.Add(1) == 1 {
							close(started)
						}
						if blocked {
							<-release
						}
						return meshclient.Summary{Head: "head"}, nil
					})
			}()
			defer cancel()
			defer once.Do(func() { close(release) })
			waitSyncWatchSignal(t, started, "startup sync")
			if blocked {
				cancel()
				select {
				case err := <-done:
					t.Fatalf("returned before durable round drained: %v", err)
				case <-time.After(100 * time.Millisecond):
				}
				once.Do(func() { close(release) })
			} else {
				waitSyncWatchSignal(t, refreshed, "post-sync discovery")
				time.Sleep(100 * time.Millisecond)
				cancel()
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("shutdown hung")
			}
			if calls.Load() != 1 {
				t.Fatalf("completion fed back into sync: %d calls", calls.Load())
			}
		})
	}
}
