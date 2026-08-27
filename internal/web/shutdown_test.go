// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/index"
)

const shutdownHelperEnv = "MESH_WEB_SIGTERM_HELPER"

func waitForOwnerFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat owner lock: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("owner lock was not published at %s", path)
}

func requireOwnerFileGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owner lock survived clean shutdown: err=%v", err)
	}
}

func TestServeContextCancellationReleasesOwnership(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- ServeContext(ctx, dir, "127.0.0.1:0", "", "", true, nil, nil, nil, nil)
	}()
	lockPath := filepath.Join(dir, ".mesh", index.OwnerLockName)
	waitForOwnerFile(t, lockPath)
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("ServeContext after cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeContext did not stop after cancellation")
	}
	requireOwnerFileGone(t, lockPath)
}

func TestOwningStartupCancellationReleasesOwnership(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		_, err := newOwningServerContext(ctx, dir,
			func(ctx context.Context, _ *index.Store, _ string) (*graph.Graph, error) {
				close(entered) // owner.lock exists and the writable Store is open
				<-ctx.Done()
				return nil, ctx.Err()
			})
		errCh <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("owning constructor did not reach the cancellable startup phase")
	}
	lockPath := filepath.Join(dir, ".mesh", index.OwnerLockName)
	waitForOwnerFile(t, lockPath)
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("owning constructor cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owning constructor did not stop after cancellation")
	}
	requireOwnerFileGone(t, lockPath)
	replacement, err := index.AcquireOwnerLock(filepath.Dir(lockPath), "replacement", false)
	if err != nil {
		t.Fatalf("immediate replacement could not acquire after startup cancellation: %v", err)
	}
	if err := replacement.Release(); err != nil {
		t.Fatalf("release replacement ownership: %v", err)
	}
}

func TestRequestDrainNeverFinishesBeforeAnActiveHandler(t *testing.T) {
	drain := newRequestDrain()
	entered := make(chan struct{})
	release := make(chan struct{})
	h := drain.wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release // deliberately ignores request cancellation, like the hazardous case
	}))
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		close(done)
	}()
	<-entered
	idle := drain.stop()
	select {
	case <-idle:
		t.Fatal("drain reported idle while the admitted handler was still running")
	default:
	}

	refused := httptest.NewRecorder()
	h.ServeHTTP(refused, httptest.NewRequest(http.MethodGet, "/late", nil))
	if refused.Code != http.StatusServiceUnavailable {
		t.Fatalf("request admitted after shutdown began: status=%d", refused.Code)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("test handler did not finish")
	}
	select {
	case <-idle:
	case <-time.After(time.Second):
		t.Fatal("drain did not become idle after the active handler returned")
	}
}

func TestServeBindFailureReleasesOwnership(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy address: %v", err)
	}
	defer occupied.Close()
	dir := t.TempDir()
	err = ServeContext(context.Background(), dir, occupied.Addr().String(), "", "", true, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("ServeContext succeeded on an occupied address")
	}
	requireOwnerFileGone(t, filepath.Join(dir, ".mesh", index.OwnerLockName))
}

func TestServeSIGTERMReleasesOwnership(t *testing.T) {
	if os.Getenv(shutdownHelperEnv) == "1" {
		dir := os.Getenv("MESH_WEB_SIGTERM_VAULT")
		if err := Serve(dir, "127.0.0.1:0", "", "", true, nil, nil, nil, nil); err != nil {
			t.Fatalf("helper Serve: %v", err)
		}
		return
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not support sending SIGTERM to the Go test subprocess")
	}

	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestServeSIGTERMReleasesOwnership$")
	cmd.Env = append(os.Environ(), shutdownHelperEnv+"=1", "MESH_WEB_SIGTERM_VAULT="+dir)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	lockPath := filepath.Join(dir, ".mesh", index.OwnerLockName)
	waitForOwnerFile(t, lockPath)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		waited = true
		t.Fatalf("signal helper: %v", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	select {
	case err := <-waitCh:
		waited = true
		if err != nil {
			t.Fatalf("SIGTERM helper exit: %v\n%s", err, output.String())
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-waitCh
		waited = true
		t.Fatalf("SIGTERM helper did not exit promptly\n%s", output.String())
	}
	requireOwnerFileGone(t, lockPath)
	replacement, err := index.AcquireOwnerLock(filepath.Dir(lockPath), "replacement", false)
	if err != nil {
		t.Fatalf("immediate replacement could not acquire ownership: %v", err)
	}
	if err := replacement.Release(); err != nil {
		t.Fatalf("release replacement ownership: %v", err)
	}
}
