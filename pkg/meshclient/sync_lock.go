// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// Same-vault sync is a transaction over the note tree, credentials, and
// sync.json. It must be serial across both goroutines and processes: mesh sync,
// mesh sync --watch, and mesh-curator can all target the same directory.
//
// The process mutex avoids relying on platform-specific same-process advisory
// lock semantics. The OS lock closes the cross-process gap and is released by
// the kernel on crash, so no stale lock file can wedge restart recovery.
type localVaultLock struct {
	mu   sync.Mutex
	refs int
}

var localVaultLocks = struct {
	sync.Mutex
	byPath map[string]*localVaultLock
}{byPath: make(map[string]*localVaultLock)}

func acquireVaultSyncLock(vaultDir string) (func(), error) {
	key, err := filepath.Abs(vaultDir)
	if err != nil {
		return nil, err
	}
	if resolved, rerr := filepath.EvalSymlinks(key); rerr == nil {
		key = resolved
	}
	key = filepath.Clean(key)

	localVaultLocks.Lock()
	local := localVaultLocks.byPath[key]
	if local == nil {
		local = &localVaultLock{}
		localVaultLocks.byPath[key] = local
	}
	local.refs++
	localVaultLocks.Unlock()
	local.mu.Lock()

	releaseLocal := func() {
		local.mu.Unlock()
		localVaultLocks.Lock()
		local.refs--
		if local.refs == 0 {
			delete(localVaultLocks.byPath, key)
		}
		localVaultLocks.Unlock()
	}

	meshDir := filepath.Join(key, ".mesh")
	if err := os.MkdirAll(meshDir, 0o700); err != nil {
		releaseLocal()
		return nil, err
	}
	lockPath := filepath.Join(meshDir, "sync.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		releaseLocal()
		return nil, err
	}
	if err := lockSyncFile(f); err != nil {
		_ = f.Close()
		releaseLocal()
		return nil, fmt.Errorf("lock sync for %s: %w", key, err)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			if err := unlockSyncFile(f); err != nil {
				slog.Warn("sync: failed to release vault file lock", "path", lockPath, "err", err)
			}
			if err := f.Close(); err != nil {
				slog.Warn("sync: failed to close vault file lock", "path", lockPath, "err", err)
			}
			releaseLocal()
		})
	}, nil
}
