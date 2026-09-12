// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/meshcfg"
)

func TestCodeRefreshThroughOwnerRejectsOverridesBeforeOpening(t *testing.T) {
	for _, flags := range [][]string{
		{"--root", "/not-an-owner-root"},
		{"--languages", "go"},
		{"--full"},
		{"--full=false"},
		{"--wait", "0s"},
		{"--wait", "-1s"},
	} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			root := t.TempDir()
			cmd := codeReindexCmd()
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(append([]string{"--through-owner", root}, flags...))
			if err := cmd.Execute(); err == nil {
				t.Fatal("owner-routed command accepted overrides or invalid wait")
			}
			if _, err := os.Stat(filepath.Join(root, ".mesh")); !os.IsNotExist(err) {
				t.Fatalf("invalid command opened or queued against a store: %v", err)
			}
		})
	}
}

func TestCodeRefreshThroughOwnerDefaultWait(t *testing.T) {
	cmd := codeReindexCmd()
	wait, err := cmd.Flags().GetDuration("wait")
	if err != nil || wait != time.Minute {
		t.Fatalf("default owner wait = %s, %v; want one minute", wait, err)
	}
}

func TestCodeRefreshThroughOwnerExpectedRootGuard(t *testing.T) {
	for _, mode := range []string{"match", "symlink match", "mismatch", "multiple configured"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			store, err := index.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			codeRoot := t.TempDir()
			if err := os.WriteFile(filepath.Join(codeRoot, "current.go"), []byte("package demo\nfunc ExpectedCheckoutSymbol() {}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			config := meshcfg.Config{Code: meshcfg.Code{Index: true, Roots: []string{codeRoot}, Languages: []string{"go"}}}
			expected := codeRoot
			wantSuccess := true
			switch mode {
			case "symlink match":
				expected = filepath.Join(t.TempDir(), "checkout")
				if err := os.Symlink(codeRoot, expected); err != nil {
					t.Fatal(err)
				}
			case "mismatch":
				expected = t.TempDir()
				wantSuccess = false
			case "multiple configured":
				config.Code.Roots = append(config.Code.Roots, t.TempDir())
				wantSuccess = false
			}
			meshDir := filepath.Join(root, ".mesh")
			if err := meshcfg.SaveConfig(meshDir, config); err != nil {
				t.Fatal(err)
			}
			cmd := codeReindexCmd()
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs([]string{"--through-owner", "--expect-root", expected, "--wait", "1s", root})
			err = cmd.Execute()
			if (err == nil) != wantSuccess {
				t.Fatalf("expected-root check: %v, want success=%v", err, wantSuccess)
			}
			if !wantSuccess && !strings.Contains(err.Error(), "--expect-root") {
				t.Fatalf("failed for an unrelated reason: %v", err)
			}
			entries, err := os.ReadDir(index.OpsDir(meshDir))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("command left queued requests: %v", entries)
			}
			reader, err := index.OpenReadOnly(root)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			hits, err := reader.SearchCode(context.Background(), "ExpectedCheckoutSymbol", 5, nil)
			if err != nil || (len(hits) == 1) != wantSuccess {
				t.Fatalf("guard/index outcome disagrees: %+v, %v; want indexed=%v", hits, err, wantSuccess)
			}
		})
	}
}

func TestCodeRefreshThroughOwnerLiveHandoff(t *testing.T) {
	root := t.TempDir()
	meshDir := filepath.Join(root, ".mesh")
	if err := os.MkdirAll(meshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, err := index.AcquireOwnerLock(meshDir, "refresh test owner", false)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Release()
	store, err := index.OpenOwned(root, owner)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	codeRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(codeRoot, "current.go"), []byte("package demo\nfunc CurrentOwnerSymbol() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := meshcfg.Config{Code: meshcfg.Code{Index: true, Roots: []string{codeRoot}, Languages: []string{"go"}}}
	if err := meshcfg.SaveConfig(meshDir, config); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	drained := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				drained <- ctx.Err()
				return
			case <-ticker.C:
				count, err := store.DrainOpsContext(ctx)
				if err != nil || count > 0 {
					drained <- err
					return
				}
			}
		}
	}()
	cmd := codeReindexCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--through-owner", "--wait", "3s", root})
	commandErr := cmd.ExecuteContext(ctx)
	// Receipt publication can precede queue-file removal. Let the owner's bounded
	// drain finish normally before canceling its context and closing its store.
	if commandErr != nil {
		cancel()
	}
	drainErr := <-drained // Always join before closing the owner's store.
	if commandErr != nil || drainErr != nil {
		t.Fatalf("CLI/owner handoff failed: command=%v drain=%v", commandErr, drainErr)
	}
	if !owner.Held() {
		t.Fatal("CLI preempted the live owner")
	}
	hits, err := store.SearchCode(context.Background(), "CurrentOwnerSymbol", 5, nil)
	if err != nil || len(hits) != 1 {
		t.Fatalf("CLI acknowledged unavailable source symbol: %+v, %v", hits, err)
	}
}
