// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type ackTraceCapture struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (c *ackTraceCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.Write(p)
}

func (c *ackTraceCapture) summaries(t *testing.T) map[string]map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]map[string]any)
	for _, line := range bytes.Split(c.b.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var r map[string]any
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatal(err)
		}
		if r["msg"] != "mesh: slow operation returned" {
			continue
		}
		// A deliberately private ID/path/title must never become a trace label.
		if strings.Contains(string(line), "private-trace-marker") {
			t.Fatalf("trace leaked note data: %s", line)
		}
		out[r["operation"].(string)] = r
	}
	return out
}

// Exercise real deadline paths, not a mock timer: a reader lock stall and a
// not-yet-indexed note must be distinguishable without changing either receipt.
func TestReaderTraceDistinguishesLockAndOwnerWait(t *testing.T) {
	for _, mode := range []string{"ack_lock", "refresh_lock", "owner_wait"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			seedIndex(t, root)
			srv, err := NewServer(root)
			if err != nil {
				t.Fatal(err)
			}
			defer srv.Close()
			if err := srv.WaitReady(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "private-trace-marker.md")
			if err := os.WriteFile(path, []byte("---\nid: private-trace-marker\ntype: note\ntitle: private-trace-marker\n---\nprivate-trace-marker\n"), 0600); err != nil {
				t.Fatal(err)
			}
			oldGraph, oldRetriever := srv.snapshot()
			if mode != "owner_wait" {
				srv.reloadMu.Lock()
				defer srv.reloadMu.Unlock()
			}
			capture := new(ackTraceCapture)
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(capture, nil)))
			defer slog.SetDefault(previous)
			// Production summaries are intentionally silent below one second.
			srv.ownerIndexTimeout = 1100 * time.Millisecond
			if mode == "refresh_lock" {
				ctx, cancel := context.WithTimeout(context.Background(), srv.ownerIndexTimeout)
				defer cancel()
				_, err = srv.refreshContext(ctx)
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("refresh error = %v", err)
				}
			} else {
				err = srv.awaitOwnerIndexed(context.Background(), "private-trace-marker", path)
				if !errors.Is(err, ErrOwnerNotIndexing) {
					t.Fatalf("ack error = %v", err)
				}
			}
			g, r := srv.snapshot()
			if g != oldGraph || r != oldRetriever {
				t.Fatal("timeout published a different reader snapshot")
			}
			summaries := capture.summaries(t)
			operation, phase := "mcp_acknowledge", "poll_wait"
			if mode == "ack_lock" {
				operation, phase = "mcp_version_refresh", "reload_lock"
			} else if mode == "refresh_lock" {
				operation, phase = "mcp_refresh", "reload_lock"
			}
			summary := summaries[operation]
			if summary == nil {
				t.Fatalf("missing %s summary: %v", operation, summaries)
			}
			phases := summary["phases"].(map[string]any)
			if duration, ok := phases[phase].(float64); !ok || duration <= 0 {
				t.Fatalf("missing timed phase %s: %v", phase, summary)
			}
			if mode != "owner_wait" && summary["last_phase"] != "reload_lock" {
				t.Fatalf("wrong blocked phase: %v", summary)
			}
			if mode == "ack_lock" {
				if ack := summaries["mcp_acknowledge"]; ack == nil || ack["last_phase"] != "version_refresh" {
					t.Fatalf("missing enclosing acknowledgement trace: %v", summaries)
				}
			}
		})
	}
}
