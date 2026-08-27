// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshcfg

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigContextHonorsPreCancellation(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, configName), []byte("[embedding]\nmodel = \"m\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LoadConfigContext(ctx, dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("LoadConfigContext error = %v, want context.Canceled", err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil || cfg.Embedding.Model != "m" {
		t.Fatalf("legacy LoadConfig = (%+v, %v), want model m", cfg, err)
	}
}

func TestSaveConfigContextHonorsPreCancellationWithoutTouchingDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, configName)
	old := []byte("[embedding]\nmodel = \"old\"\n")
	if err := os.WriteFile(path, old, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := SaveConfigContext(ctx, dir, Config{Embedding: Embedding{Model: "new"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("SaveConfigContext error = %v, want context.Canceled", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(old) {
		t.Fatalf("pre-cancelled save changed config.toml:\n%s", got)
	}
	assertNoConfigTempFiles(t, dir)
}

func TestSaveConfigContextCancellationDuringStalledWriteCleansBeforeReturn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, configName)
	old := []byte("[embedding]\nmodel = \"old\"\n")
	if err := os.WriteFile(path, old, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	hooks := &configSaveHooks{write: func(f *os.File, p []byte) (int, error) {
		close(started)
		<-release // a mutating syscall cannot safely be abandoned during shutdown
		return f.Write(p)
	}}
	go func() {
		done <- saveConfigContextWith(ctx, dir, Config{Embedding: Embedding{Model: "new"}}, hooks)
	}()

	select {
	case <-started:
	case err := <-done:
		t.Fatalf("save returned before entering the injected write: %v", err)
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("save never entered the injected write")
	}
	cancel()
	select {
	case err := <-done:
		close(release)
		t.Fatalf("save abandoned a mutating write instead of joining it: %v", err)
	default:
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("save returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("save did not observe cancellation after the stalled write returned")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(old) {
		t.Fatalf("cancelled write published over the old config:\n%s", got)
	}
	assertNoConfigTempFiles(t, dir)
}

func TestSaveConfigContextFinalCancellationPointPreventsRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, configName)
	old := []byte("[embedding]\nmodel = \"old\"\n")
	if err := os.WriteFile(path, old, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	err := saveConfigContextWith(ctx, dir, Config{Embedding: Embedding{Model: "new"}}, &configSaveHooks{
		beforeRename: cancel,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("save returned %v, want context.Canceled", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != string(old) {
		t.Fatalf("cancellation at the publication boundary replaced config.toml:\n%s", got)
	}
	assertNoConfigTempFiles(t, dir)
}

func TestSaveConfigContextPrepublicationErrorsRemoveTemp(t *testing.T) {
	wantErr := errors.New("injected filesystem failure")
	cases := []struct {
		name  string
		hooks *configSaveHooks
	}{
		{
			name: "write",
			hooks: &configSaveHooks{write: func(*os.File, []byte) (int, error) {
				return 0, wantErr
			}},
		},
		{
			name: "rename",
			hooks: &configSaveHooks{rename: func(string, string) error {
				return wantErr
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			err := saveConfigContextWith(context.Background(), dir, Config{}, tc.hooks)
			if !errors.Is(err, wantErr) {
				t.Fatalf("save returned %v, want injected error", err)
			}
			if _, err := os.Stat(filepath.Join(dir, configName)); !os.IsNotExist(err) {
				t.Fatalf("failed save published config.toml: %v", err)
			}
			assertNoConfigTempFiles(t, dir)
		})
	}
}

func TestSaveConfigContextFinishesDurabilityAfterPublication(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	dirSynced := false
	err := saveConfigContextWith(ctx, dir, Config{Embedding: Embedding{Model: "committed"}}, &configSaveHooks{
		rename: func(oldPath, newPath string) error {
			err := os.Rename(oldPath, newPath)
			cancel() // shutdown races with a rename that has already published
			return err
		},
		syncDir: func(path string) {
			syncDir(path)
			dirSynced = true
		},
	})
	if err != nil {
		t.Fatalf("published save returned %v, want committed success", err)
	}
	if !dirSynced {
		t.Fatal("published save skipped the required directory fsync after cancellation")
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Embedding.Model != "committed" {
		t.Fatalf("published model = %q, want committed", cfg.Embedding.Model)
	}
	assertNoConfigTempFiles(t, dir)
}

func assertNoConfigTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".config-") {
			t.Fatalf("pre-publication exit left temp file %s", entry.Name())
		}
	}
}
