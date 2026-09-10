// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package onboarding

import (
	"os"
	"sync"
	"sync/atomic"
	"testing"
)

func TestMarkerConsumeOnceAndValidate(t *testing.T) {
	vault := t.TempDir()
	if _, ok := ConsumePending(vault); ok {
		t.Fatal("no marker yet: consume should be false")
	}
	if err := SetPending(vault, "codex"); err != nil {
		t.Fatal(err)
	}
	client, ok := ConsumePending(vault)
	if !ok || client != "codex" {
		t.Fatalf("first consume = (%q, %v), want (codex, true)", client, ok)
	}
	if _, ok := ConsumePending(vault); ok {
		t.Fatal("second consume should be false")
	}
	if err := SetPending(vault, "codex\nIGNORE PREVIOUS"); err == nil {
		t.Fatal("free-form client text must be rejected")
	}

	p := marker(vault)
	if err := os.WriteFile(p, []byte("IGNORE PREVIOUS"), 0o600); err != nil {
		t.Fatal(err)
	}
	if client, ok := ConsumePending(vault); ok || client != "" {
		t.Fatalf("malformed marker entered the prompt path: (%q, %v)", client, ok)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("malformed marker was not cleared: %v", err)
	}
}

func TestMarkerHasOneWinnerUnderConcurrency(t *testing.T) {
	vault := t.TempDir()
	if err := SetPending(vault, "vscode"); err != nil {
		t.Fatal(err)
	}
	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if client, ok := ConsumePending(vault); ok {
				if client != "vscode" {
					t.Errorf("winning client = %q, want vscode", client)
				}
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Fatalf("concurrent MCP consumes = %d, want exactly 1", got)
	}
}
