// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOwnerLockAdmitsOneDeclaredOwner: two watchers against one mesh.db is the contention
// the single-writer split removed, and until this lock existed nothing checked. Starting a
// second one by hand was routine, because the only symptom is latency.
func TestOwnerLockAdmitsOneDeclaredOwner(t *testing.T) {
	dir := t.TempDir()

	first, err := AcquireOwnerLock(dir, "mesh watch", false)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	defer first.Release()

	_, err = AcquireOwnerLock(dir, "mesh sync --watch", false)
	if !errors.Is(err, ErrOwnerHeld) {
		t.Fatalf("second declared owner got %v, want ErrOwnerHeld", err)
	}
	// The message has to name the process a human then has to go and find.
	if !strings.Contains(err.Error(), "mesh watch") {
		t.Errorf("the refusal does not name the holder: %v", err)
	}

	info, live := OwnerStatus(dir)
	if !live || info.Role != "mesh watch" {
		t.Fatalf("OwnerStatus = %+v live=%v, want the live `mesh watch` claim", info, live)
	}
}

// TestDeclaredOwnerPreemptsAnOpportunisticClaim: `mesh mcp` elects itself so the shipped
// config works on its own, but the operator's explicit command must still win, and the
// displaced holder must be able to SEE that it lost.
func TestDeclaredOwnerPreemptsAnOpportunisticClaim(t *testing.T) {
	dir := t.TempDir()

	elected, err := AcquireOwnerLock(dir, "mesh mcp", true)
	if err != nil {
		t.Fatalf("elected claim: %v", err)
	}
	defer elected.Release()
	if !elected.Held() {
		t.Fatal("a fresh claim does not report itself held")
	}

	declared, err := AcquireOwnerLock(dir, "mesh watch", false)
	if err != nil {
		t.Fatalf("a declared owner must preempt an opportunistic claim, got %v", err)
	}
	defer declared.Release()

	if elected.Held() {
		t.Error("the preempted holder still thinks it owns the vault, so two processes will index it")
	}
	if !declared.Held() {
		t.Error("the declared owner does not hold what it just took")
	}
	// Releasing the loser must not take the winner's claim away with it.
	if err := elected.Release(); err != nil {
		t.Fatalf("release after preemption: %v", err)
	}
	if _, live := OwnerStatus(dir); !live {
		t.Error("releasing a preempted claim removed the live owner's lock")
	}
}

// TestOpportunisticClaimsDoNotPreemptEachOther: N agent windows open on one vault must
// settle on one owner, not take it from each other in a loop.
func TestOpportunisticClaimsDoNotPreemptEachOther(t *testing.T) {
	dir := t.TempDir()
	first, err := AcquireOwnerLock(dir, "mesh mcp", true)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()

	if _, err := AcquireOwnerLock(dir, "mesh mcp", true); !errors.Is(err, ErrOwnerHeld) {
		t.Fatalf("a second window got %v, want ErrOwnerHeld", err)
	}
	if !first.Held() {
		t.Error("the first window lost its claim to a peer that should have yielded")
	}
}

// TestADeadOwnersLockIsReclaimed: a crashed watcher (or a laptop that was power-cycled)
// leaves its lock behind. If that wedged the vault, the fix would be worse than the
// defect: the user's agent would never index again and nothing would say why.
func TestADeadOwnersLockIsReclaimed(t *testing.T) {
	dir := t.TempDir()
	stale := OwnerInfo{
		PID: deadPID(t), Host: hostname(t), Role: "mesh watch",
		StartedAt: time.Now().Add(-time.Hour).Unix(), Nonce: "deadbeefdeadbeef",
	}
	b, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, OwnerLockName), b, 0o600); err != nil {
		t.Fatal(err)
	}
	// Old enough that even a host that cannot check pids calls it abandoned.
	old := time.Now().Add(-2 * OwnerLockStaleAfter)
	if err := os.Chtimes(filepath.Join(dir, OwnerLockName), old, old); err != nil {
		t.Fatal(err)
	}

	if _, live := OwnerStatus(dir); live {
		t.Fatal("a dead owner's lock is reported as a live owner")
	}
	lock, err := AcquireOwnerLock(dir, "mesh mcp", true)
	if err != nil {
		t.Fatalf("a dead owner's lock must be reclaimable, got %v", err)
	}
	defer lock.Release()
	if !lock.Held() {
		t.Error("reclaimed the lock but do not hold it")
	}
}

// TestReleaseLeavesNoOwner: the next process to start must be able to elect itself, and
// mesh doctor must report the vault as unowned rather than naming a process that exited.
func TestReleaseLeavesNoOwner(t *testing.T) {
	dir := t.TempDir()
	lock, err := AcquireOwnerLock(dir, "mesh mcp", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, live := OwnerStatus(dir); live {
		t.Fatal("the vault still reports an owner after the only holder released it")
	}
	if err := lock.Release(); err != nil {
		t.Errorf("a second release must be a no-op, got %v", err)
	}
}

// TestNoOwnerRemedyNamesTheFix: this string is the only thing a stranger gets when their
// vault has stopped indexing, so it has to say what to run.
func TestNoOwnerRemedyNamesTheFix(t *testing.T) {
	for _, root := range []string{"", "/tmp/my-vault"} {
		msg := NoOwnerRemedy(root)
		for _, want := range []string{"no owning writer detected", "mesh mcp --vault", "mesh watch"} {
			if !strings.Contains(msg, want) {
				t.Errorf("NoOwnerRemedy(%q) does not mention %q: %s", root, want, msg)
			}
		}
		if root != "" && !strings.Contains(msg, root) {
			t.Errorf("NoOwnerRemedy(%q) does not name the vault: %s", root, msg)
		}
	}
}

func hostname(t *testing.T) string {
	t.Helper()
	h, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// deadPID returns a pid nothing is running under: a child started and reaped. Picking a
// number out of the air races a real process on a busy machine, and on this one that
// would be a flake that only ever appears in CI.
func deadPID(t *testing.T) int {
	t.Helper()
	// The test binary itself, told to run no tests: portable, and it exits immediately.
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Skipf("cannot start a probe process here: %v", err)
	}
	return cmd.ProcessState.Pid()
}
