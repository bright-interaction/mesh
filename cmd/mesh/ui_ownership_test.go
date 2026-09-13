// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/index"
)

// Exercise the real Cobra command and server constructors, rather than restating
// flag parsing in a mock. An occupied ephemeral loopback port makes startup stop
// deterministically AFTER opening the index, without a service, sleeps or global
// stdout capture. All index writes are confined to the test's temporary vault.
func TestUIOwnershipFlagOverridesEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		flag string
		owns bool
	}{
		{"explicit false overrides inherited true", "1", "--own-index=false", false},
		{"unset flag honors inherited true", "1", "", true},
		{"explicit true overrides inherited false", "0", "--own-index=true", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MESH_UI_OWN_INDEX", tc.env)
			for _, key := range []string{"MESH_UI_TOKEN", "MESH_UI_HUB_DB", "MESH_UI_BASE_PATH", "MESH_WEB_DEV"} {
				t.Setenv(key, "")
			}
			for _, existingOwner := range []bool{true, false} {
				name := "unindexed vault"
				if existingOwner {
					name = "existing owner"
				}
				t.Run(name, func(t *testing.T) {
					root := t.TempDir()
					meshDir := filepath.Join(root, ".mesh")
					var owner *index.OwnerLock
					if existingOwner {
						var err error
						owner, err = index.AcquireOwnerLock(meshDir, "mesh watch (UI flag test)", false)
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() {
							if err := owner.Release(); err != nil {
								t.Error(err)
							}
						})
						store, err := index.OpenOwned(root, owner)
						if err != nil {
							t.Fatal(err)
						}
						if err := store.Close(); err != nil {
							t.Fatal(err)
						}
					}
					if err := os.WriteFile(filepath.Join(root, "not-indexed.md"), []byte("---\nid: not-indexed\ntype: note\n---\n# Not indexed\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					listener, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					defer listener.Close()
					args := []string{root, "--addr", listener.Addr().String()}
					if tc.flag != "" {
						args = append(args, tc.flag)
					}
					cmd := uiCmd()
					cmd.SilenceErrors, cmd.SilenceUsage = true, true
					cmd.SetOut(io.Discard)
					cmd.SetErr(io.Discard)
					cmd.SetArgs(args)
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					err = cmd.ExecuteContext(ctx)
					if ctx.Err() != nil {
						t.Fatalf("startup exceeded its bound: %v", ctx.Err())
					}
					switch {
					case existingOwner && tc.owns:
						if !errors.Is(err, index.ErrOwnerHeld) {
							t.Fatalf("owning mode must refuse the existing owner, got %v", err)
						}
					case !existingOwner && !tc.owns:
						if !errors.Is(err, index.ErrNoIndexYet) {
							t.Fatalf("read-only mode must not initialize an index, got %v", err)
						}
					default:
						if !errors.Is(err, syscall.EADDRINUSE) {
							t.Fatalf("expected initialized viewer to reach occupied listener, got %v", err)
						}
					}
					if existingOwner {
						if !owner.Held() {
							t.Fatal("UI command replaced or released the existing owner")
						}
					} else if info, live := index.OwnerStatus(meshDir); live {
						t.Fatalf("failed startup leaked an owner claim: %+v", info)
					}
					if existingOwner || tc.owns {
						store, err := index.OpenReadOnly(root)
						if err != nil {
							t.Fatal(err)
						}
						defer store.Close()
						want := 0
						if !existingOwner && tc.owns {
							want = 1
						}
						if count, err := store.Count("notes"); err != nil || count != want {
							t.Fatalf("indexed notes = %d, want %d (error %v)", count, want, err)
						}
					}
				})
			}
		})
	}
}
