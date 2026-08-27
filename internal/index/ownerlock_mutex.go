// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
)

const ownerMetadataGuardName = "owner.lock.guard"

// flock/LockFileEx semantics for two separately-opened handles in one process vary by
// platform, so serialize locally as well as across processes. The local lock is keyed by
// vault: ownership leases now span SQLite COMMIT, and a single global mutex would make a
// long transaction in one vault stall every unrelated vault in the process (and could
// deadlock callbacks that legitimately touch a second vault).
var ownerMetadataProcessLocks = struct {
	sync.Mutex
	byPath map[string]*ownerMetadataProcessLock
}{byPath: make(map[string]*ownerMetadataProcessLock)}

type ownerMetadataProcessLock struct {
	mu   sync.Mutex
	refs int
}

type ownerMetadataLock struct {
	f          *os.File
	processKey string
	process    *ownerMetadataProcessLock
}

func acquireOwnerMetadataLock(meshDir string, create bool) (*ownerMetadataLock, error) {
	processKey, err := filepath.Abs(filepath.Clean(meshDir))
	if err != nil {
		return nil, err
	}
	ownerMetadataProcessLocks.Lock()
	process := ownerMetadataProcessLocks.byPath[processKey]
	if process == nil {
		process = &ownerMetadataProcessLock{}
		ownerMetadataProcessLocks.byPath[processKey] = process
	}
	process.refs++
	ownerMetadataProcessLocks.Unlock()
	process.mu.Lock()
	releaseProcess := func() {
		process.mu.Unlock()
		ownerMetadataProcessLocks.Lock()
		process.refs--
		if process.refs == 0 {
			delete(ownerMetadataProcessLocks.byPath, processKey)
		}
		ownerMetadataProcessLocks.Unlock()
	}
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	f, err := os.OpenFile(filepath.Join(meshDir, ownerMetadataGuardName), flags, 0o600)
	if err != nil {
		releaseProcess()
		return nil, err
	}
	if err := lockOwnerMetadataFile(f); err != nil {
		_ = f.Close()
		releaseProcess()
		return nil, err
	}
	return &ownerMetadataLock{f: f, processKey: processKey, process: process}, nil
}

func (l *ownerMetadataLock) release() error {
	unlockErr := unlockOwnerMetadataFile(l.f)
	closeErr := l.f.Close()
	l.process.mu.Unlock()
	ownerMetadataProcessLocks.Lock()
	l.process.refs--
	if l.process.refs == 0 {
		delete(ownerMetadataProcessLocks.byPath, l.processKey)
	}
	ownerMetadataProcessLocks.Unlock()
	return errors.Join(unlockErr, closeErr)
}

func withOwnerMetadataLock(meshDir string, fn func() error) error {
	l, err := acquireOwnerMetadataLock(meshDir, true)
	if err != nil {
		return err
	}
	fnErr := fn()
	return errors.Join(fnErr, l.release())
}

func withExistingOwnerMetadataLock(meshDir string, fn func() error) error {
	l, err := acquireOwnerMetadataLock(meshDir, false)
	if err != nil {
		return err
	}
	fnErr := fn()
	return errors.Join(fnErr, l.release())
}
