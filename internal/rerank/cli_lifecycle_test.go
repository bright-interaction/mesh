//go:build linux || darwin

// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package rerank

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Re-exec only the test binary. No provider executable, auth or network access.
func TestCLILifecycleHelper(t *testing.T) {
	if os.Getenv("MESH_LLM_CHILD") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) != 3 {
		os.Exit(81)
	}
	mode, marker := args[1], args[2]
	switch mode {
	case "stdout", "stderr":
		out := os.Stdout
		if mode == "stderr" {
			out = os.Stderr
			fmt.Fprint(os.Stdout, "[1,0]") // Valid stdout must not mask diagnostic overflow.
		}
		chunk := strings.Repeat("x", 8192)
		for {
			if _, err := io.WriteString(out, chunk); err != nil {
				os.Exit(82)
			}
		}
	case "hang", "rank-hang":
		if mode == "rank-hang" {
			fmt.Fprint(os.Stdout, "[1,0]")
		}
		if err := os.WriteFile(marker, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			os.Exit(83)
		}
		for {
			time.Sleep(time.Second)
		}
	case "orphan-pipes", "orphan-closed":
		child := exec.Command(os.Args[0], "-test.run=^TestCLILifecycleHelper$", "--", "hang", marker)
		if mode == "orphan-pipes" {
			child.Stdout, child.Stderr = os.Stdout, os.Stderr
		}
		if child.Start() != nil {
			os.Exit(84)
		}
		limit := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(marker); err == nil {
				break
			}
			if time.Now().After(limit) {
				os.Exit(85)
			}
			time.Sleep(time.Millisecond)
		}
		fmt.Fprint(os.Stdout, "[1,0]")
	case "rank":
		if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
			os.Exit(86)
		}
		f, err := os.OpenFile(marker, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			os.Exit(87)
		}
		f.WriteString("x")
		f.Close()
		fmt.Fprint(os.Stdout, "[1,0]")
	default:
		os.Exit(88)
	}
	os.Exit(0)
}

func lifecycleCLI(t *testing.T, mode string) (*CLI, string) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "marker")
	c := &CLI{provider: "test", model: "tiny", argv: []string{os.Args[0], "-test.run=^TestCLILifecycleHelper$", "--", mode, marker},
		timeout: 5 * time.Second, candidateCap: 2, cardCharCap: 200,
		cooldown: time.Minute, cache: make(map[[32]byte][]Result)}
	return c, marker
}

func awaitHelper(t *testing.T, marker string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(marker); err == nil {
			if pid, err := strconv.Atoi(string(data)); err == nil && pid > 1 {
				return pid
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("helper did not start")
	return 0
}

func assertHelperStopped(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		// Linux init owns orphan reaping; a zombie is no longer executing.
		if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
			end := strings.LastIndex(string(data), ") ")
			if end >= 0 && strings.HasPrefix(string(data)[end+2:], "Z ") {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("helper PID %d still running", pid)
}

func TestCLICaptureBoundsDuringCopy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	capture := &cliCapture{limit: 32, cancel: cancel}
	if _, bypass := any(capture).(io.ReaderFrom); bypass {
		t.Fatal("ReaderFrom would bypass the capped Write method")
	}
	_, err := io.Copy(capture, strings.NewReader(strings.Repeat("x", 1000)))
	if !errors.Is(err, errCLIOutputLimit) || capture.Len() != 32 || ctx.Err() == nil {
		t.Fatalf("capture len=%d cancelled=%v err=%v", capture.Len(), ctx.Err(), err)
	}
}

func TestCLIOutputFloods(t *testing.T) {
	for _, mode := range []string{"stdout", "stderr"} {
		t.Run(mode, func(t *testing.T) {
			c, _ := lifecycleCLI(t, mode)
			start := time.Now()
			out, diagnostic, err := c.run(context.Background(), "fixture")
			if !errors.Is(err, errCLIOutputLimit) || out != "" || len(diagnostic) > maxCLIStderrBytes || time.Since(start) > 4*time.Second {
				t.Fatalf("flood not bounded: out=%d stderr=%d elapsed=%s err=%v", len(out), len(diagnostic), time.Since(start), err)
			}
		})
	}
}

func TestCLIDescendantsStoppedAfterParentExit(t *testing.T) {
	for _, mode := range []string{"orphan-pipes", "orphan-closed"} {
		t.Run(mode, func(t *testing.T) {
			c, marker := lifecycleCLI(t, mode)
			start := time.Now()
			out, _, err := c.run(context.Background(), "fixture")
			if time.Since(start) > 4*time.Second {
				t.Fatal("inherited pipes exceeded shutdown bound")
			}
			if mode == "orphan-pipes" && (err == nil || out != "") {
				t.Fatal("incomplete pipe shutdown accepted as success")
			}
			if mode == "orphan-closed" && (err != nil || out != "[1,0]") {
				t.Fatalf("successful parent failed: %q, %v", out, err)
			}
			assertHelperStopped(t, awaitHelper(t, marker))
		})
	}
}

func TestCLIActiveCancellationDoesNotPublishOrTripCircuit(t *testing.T) {
	for _, mode := range []string{"hang", "rank-hang"} {
		t.Run(mode, func(t *testing.T) {
			c, marker := lifecycleCLI(t, mode)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, _, err := c.RerankCandidatesMeasured(ctx, "q", []Candidate{{Title: "one"}, {Title: "two"}})
				done <- err
			}()
			pid := awaitHelper(t, marker)
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) || len(c.cache) != 0 || !c.openUntil.IsZero() {
					t.Fatalf("cancelled call published state: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cancelled call did not return")
			}
			assertHelperStopped(t, pid)
		})
	}
}

func TestCLIQueuedCancellation(t *testing.T) {
	c, _ := lifecycleCLI(t, "rank")
	if err := c.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { <-c.admission }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, stats, err := c.RerankCandidatesMeasured(ctx, "q", []Candidate{{Title: "one"}})
	if !errors.Is(err, context.DeadlineExceeded) || stats.Called || stats.CacheHit || time.Since(start) > time.Second {
		t.Fatalf("queue did not cancel: %+v %v", stats, err)
	}
}

func TestCLIInternalTimeoutStopsChildAndTripsCircuit(t *testing.T) {
	c, marker := lifecycleCLI(t, "hang")
	c.timeout = 2 * time.Second
	done := make(chan error, 1)
	go func() {
		_, _, err := c.RerankCandidatesMeasured(context.Background(), "q", []Candidate{{Title: "one"}})
		done <- err
	}()
	pid := awaitHelper(t, marker)
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || len(c.cache) != 0 || c.openUntil.IsZero() {
			t.Fatalf("internal timeout did not fail closed: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("internal timeout did not return")
	}
	assertHelperStopped(t, pid)
	_, stats, err := c.RerankCandidatesMeasured(context.Background(), "q", []Candidate{{Title: "one"}})
	if !errors.Is(err, ErrCircuitOpen) || stats.Called || !stats.CircuitOpen {
		t.Fatalf("timeout did not suppress retry: %+v %v", stats, err)
	}
}

func TestCLIConcurrentIdenticalCallsStillCollapse(t *testing.T) {
	c, marker := lifecycleCLI(t, "rank")
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			_, err := c.RerankCandidates(context.Background(), "q", []Candidate{{Title: "one"}, {Title: "two"}})
			if err != nil {
				t.Errorf("rank: %v", err)
			}
		})
	}
	wg.Wait()
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "x" {
		t.Fatalf("identical requests did not collapse: %q %v", data, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, stats, err := c.RerankCandidatesMeasured(ctx, "q", []Candidate{{Title: "one"}, {Title: "two"}})
	if !errors.Is(err, context.Canceled) || stats.CacheHit || stats.Called {
		t.Fatalf("cancelled cache access accepted: %+v %v", stats, err)
	}
}
