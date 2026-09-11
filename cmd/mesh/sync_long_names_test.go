// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/merge"
	"github.com/bright-interaction/mesh/internal/watch"
	"github.com/bright-interaction/mesh/pkg/meshclient"
)

func TestLongConflictCanBeListedAndResolved(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, strings.Repeat("a", 247)+".md")
	sibling := merge.SiblingPath(base, time.Now(), "alice", []byte("mine"))
	if err := os.MkdirAll(filepath.Dir(sibling), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base, []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sibling, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runSub(t, conflictsListCmd(), root, "--json")
	if err != nil || !strings.Contains(out, filepath.Base(base)) {
		t.Fatalf("list lost base: %s %v", out, err)
	}
	if _, err := runSub(t, conflictsResolveCmd(), sibling, root, "--keep-base"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sibling); !os.IsNotExist(err) {
		t.Fatalf("sibling remains: %v", err)
	}
	b, err := os.ReadFile(base)
	if err != nil || string(b) != "base" {
		t.Fatalf("base lost: %q %v", b, err)
	}
}

func TestDiscoveryIndexesLocalNoteBeforeFailingSync(t *testing.T) {
	for _, reason := range []string{watch.ReasonStartup, watch.ReasonTick, watch.ReasonChange} {
		t.Run(reason, func(t *testing.T) {
			root := t.TempDir()
			store, err := index.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			live := index.NewLiveIndexer(store, root)
			if _, err := live.Reconcile(true); err != nil {
				t.Fatal(err)
			}
			body := "---\nid: local-before-sync\ntype: note\ntitle: Local before sync\n---\nSaved locally.\n"
			if err := os.WriteFile(filepath.Join(root, "local.md"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			hubCalled := false
			_, err = syncWatchPass(watch.Pass{Reason: reason}, true,
				func(_ []string, authoritative bool) error { _, err := live.Reconcile(authoritative); return err },
				func() (meshclient.Summary, error) {
					hubCalled = true
					if _, err := store.NotePath("local-before-sync"); err != nil {
						t.Errorf("network began before local write was queryable: %v", err)
					}
					return meshclient.Summary{}, syscall.ENAMETOOLONG
				})
			if !hubCalled || !errors.Is(err, syscall.ENAMETOOLONG) {
				t.Fatalf("sync failure hidden: %v", err)
			}
			if _, err := store.NotePath("local-before-sync"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
