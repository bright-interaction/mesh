// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOwnerStatusDoesNotCreateOrMutateMeshMetadata(t *testing.T) {
	meshDir := filepath.Join(t.TempDir(), ".mesh")
	if _, live := OwnerStatus(meshDir); live {
		t.Fatal("missing owner reported live")
	}
	if _, err := os.Stat(meshDir); !os.IsNotExist(err) {
		t.Fatalf("read-only status created .mesh: %v", err)
	}

	if err := os.Mkdir(meshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	info := OwnerInfo{PID: os.Getpid(), Host: hostname(t), Role: "fixture", StartedAt: time.Now().Unix(), Nonce: "status-only"}
	body, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(meshDir, OwnerLockName), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, live := OwnerStatus(meshDir); !live || got.Nonce != info.Nonce {
		t.Fatalf("status on existing read-only metadata = %+v live=%v", got, live)
	}
	if _, err := os.Stat(filepath.Join(meshDir, ownerMetadataGuardName)); !os.IsNotExist(err) {
		t.Fatalf("OwnerStatus created its coordination file: %v", err)
	}
}

func TestHeldDoesNotRecreateMissingCoordinationMetadata(t *testing.T) {
	dir := t.TempDir()
	lock, err := AcquireOwnerLock(dir, "fixture", true)
	if err != nil {
		t.Fatal(err)
	}
	lock.stopOnce.Do(func() {
		close(lock.stop)
		<-lock.done
	})
	guardPath := filepath.Join(dir, ownerMetadataGuardName)
	if err := os.Remove(guardPath); err != nil {
		t.Fatal(err)
	}
	if lock.Held() {
		t.Fatal("Held authorized a claim whose coordination metadata is unavailable")
	}
	if _, err := os.Stat(guardPath); !os.IsNotExist(err) {
		t.Fatalf("Held recreated coordination metadata: %v", err)
	}
	_ = os.Remove(filepath.Join(dir, OwnerLockName))
}

func TestOwnedOpenHoldsALeaseThroughSchemaCommitAndLaterWrites(t *testing.T) {
	root := t.TempDir()
	meshDir := filepath.Join(root, ".mesh")
	transient, err := AcquireOwnerLock(meshDir, "mesh sync", true)
	if err != nil {
		t.Fatal(err)
	}
	defer transient.Release()

	started := make(chan struct{})
	acquired := make(chan *OwnerLock, 1)
	store, err := openOwnedWithHook(root, transient, func() {
		go func() {
			close(started)
			lock, _ := AcquireOwnerLock(meshDir, "mesh watch", false)
			acquired <- lock
		}()
		<-started
		select {
		case replacement := <-acquired:
			if replacement != nil {
				_ = replacement.Release()
			}
			t.Fatal("declared owner preempted while the old owner still held its schema lease")
		default:
		}
	})
	if err != nil {
		t.Fatalf("owned open: %v", err)
	}
	replacement := <-acquired
	if replacement == nil || !replacement.Held() {
		t.Fatal("replacement owner did not acquire after schema lease released")
	}
	defer replacement.Release()

	if err := os.WriteFile(filepath.Join(root, "after-takeover.md"), []byte("---\nid: after-takeover\ntype: note\n---\n# After takeover\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Reconcile(store, root); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("preempted store committed after takeover: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// The winner can open and build the index after the old store has become read-only.
	winner, err := OpenOwned(root, replacement)
	if err != nil {
		t.Fatalf("replacement owner could not open: %v", err)
	}
	if err := winner.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOwnedTransactionIsLinearizedWithTakeoverThroughCommit(t *testing.T) {
	root := t.TempDir()
	meshDir := filepath.Join(root, ".mesh")
	old, err := AcquireOwnerLock(meshDir, "mesh mcp", true)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Release()
	store, err := OpenOwned(root, old)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	started := make(chan struct{})
	acquired := make(chan *OwnerLock, 1)
	err = store.Write(func(tx *sql.Tx) error {
		go func() {
			close(started)
			lock, _ := AcquireOwnerLock(meshDir, "mesh watch", false)
			acquired <- lock
		}()
		<-started
		select {
		case replacement := <-acquired:
			if replacement != nil {
				_ = replacement.Release()
			}
			t.Fatal("takeover passed the ownership lease before transaction commit")
		default:
		}
		_, err := tx.Exec(`INSERT INTO metrics(key,value) VALUES('lease_commit',1) ON CONFLICT(key) DO UPDATE SET value=excluded.value`)
		return err
	})
	if err != nil {
		t.Fatalf("leased transaction: %v", err)
	}
	replacement := <-acquired
	if replacement == nil || !replacement.Held() {
		t.Fatal("replacement did not acquire after commit released the lease")
	}
	defer replacement.Release()
	if err := store.Write(func(*sql.Tx) error { return nil }); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("preempted store accepted a later transaction: %v", err)
	}
}

func TestOwnershipLeaseInOneVaultDoesNotBlockAnotherVault(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	ownerA, err := AcquireOwnerLock(filepath.Join(rootA, ".mesh"), "vault-a", true)
	if err != nil {
		t.Fatal(err)
	}
	defer ownerA.Release()
	ownerB, err := AcquireOwnerLock(filepath.Join(rootB, ".mesh"), "vault-b", true)
	if err != nil {
		t.Fatal(err)
	}
	defer ownerB.Release()

	releaseA, ok := ownerA.acquireLease()
	if !ok {
		t.Fatal("could not lease vault A")
	}
	defer releaseA()

	acquiredB := make(chan func(), 1)
	go func() {
		releaseB, ok := ownerB.acquireLease()
		if !ok {
			acquiredB <- nil
			return
		}
		acquiredB <- releaseB
	}()
	select {
	case releaseB := <-acquiredB:
		if releaseB == nil {
			t.Fatal("vault B lost its independent claim")
		}
		releaseB()
	case <-time.After(time.Second):
		t.Fatal("vault A's ownership lease serialized unrelated vault B")
	}
}

func TestLegacyWritableOpensFailClosedBesideADeclaredOwner(t *testing.T) {
	root := t.TempDir()
	seed, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	owner, err := AcquireOwnerLock(filepath.Join(root, ".mesh"), "mesh watch", false)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Release()

	if store, err := Open(root); !errors.Is(err, ErrOwnerHeld) {
		if store != nil {
			_ = store.Close()
		}
		t.Fatalf("Open beside a live owner = %v, want ErrOwnerHeld", err)
	}
	if store, err := OpenCurrent(root); !errors.Is(err, ErrOwnerHeld) {
		if store != nil {
			_ = store.Close()
		}
		t.Fatalf("OpenCurrent beside a live owner = %v, want ErrOwnerHeld", err)
	}
	if store, _, err := OpenRebuild(root); !errors.Is(err, ErrOwnerHeld) {
		if store != nil {
			_ = store.Close()
		}
		t.Fatalf("OpenRebuild beside a live owner = %v, want ErrOwnerHeld", err)
	}
}

func TestLegacyStoreYieldsBeforeANewOwnerCanCommit(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner, err := AcquireOwnerLock(filepath.Join(root, ".mesh"), "mesh watch", false)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Release()
	if err := store.Write(func(*sql.Tx) error { return nil }); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("legacy store wrote after an owner claimed the vault: %v", err)
	}
}

func TestOneShotOwnerNeitherPreemptsNorCanBePreempted(t *testing.T) {
	dir := t.TempDir()
	elected, err := AcquireOwnerLock(dir, "mesh mcp", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireOneShotOwnerLock(dir, "mesh embed"); !errors.Is(err, ErrOwnerHeld) {
		t.Fatalf("one-shot command preempted a live MCP owner: %v", err)
	}
	if !elected.Held() {
		t.Fatal("failed one-shot acquisition displaced the elected owner")
	}
	if err := elected.Release(); err != nil {
		t.Fatal(err)
	}

	oneShot, err := AcquireOneShotOwnerLock(dir, "mesh embed")
	if err != nil {
		t.Fatal(err)
	}
	defer oneShot.Release()
	if _, err := AcquireOwnerLock(dir, "mesh watch", false); !errors.Is(err, ErrOwnerHeld) {
		t.Fatalf("declared owner preempted an active bounded DB operation: %v", err)
	}
}

func TestReleaseCannotRemoveAReplacementBetweenNonceCheckAndDelete(t *testing.T) {
	dir := t.TempDir()
	old, err := AcquireOwnerLock(dir, "mesh mcp", true)
	if err != nil {
		t.Fatal(err)
	}
	// Stop the heartbeat so this test controls the only old-claim metadata operation.
	old.stopOnce.Do(func() {
		close(old.stop)
		<-old.done
	})

	started := make(chan struct{})
	acquired := make(chan *OwnerLock, 1)
	releaseErr := old.releaseClaim(func() {
		go func() {
			close(started)
			lock, _ := AcquireOwnerLock(dir, "mesh watch", false)
			acquired <- lock
		}()
		<-started
		select {
		case lock := <-acquired:
			if lock != nil {
				_ = lock.Release()
			}
			t.Fatal("replacement acquired while old Release was between nonce check and delete")
		default:
		}
	})
	if releaseErr != nil {
		t.Fatal(releaseErr)
	}
	replacement := <-acquired
	if replacement == nil || !replacement.Held() {
		t.Fatal("the replacement owner was deleted by the old holder's Release")
	}
	defer replacement.Release()
}
