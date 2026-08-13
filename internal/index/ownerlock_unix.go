// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

//go:build !windows

package index

import (
	"errors"
	"syscall"
)

// processAlive asks the OS whether pid still exists. known=false means the question
// could not be answered and the caller must fall back to the lock's heartbeat.
//
// Signal 0 performs the permission and existence checks and delivers nothing, so this
// costs a syscall and disturbs the target not at all. EPERM means the process is there
// and owned by another user, which is still "alive" for our purposes.
func processAlive(pid int) (alive, known bool) {
	if pid <= 0 {
		return false, true
	}
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil:
		return true, true
	case errors.Is(err, syscall.EPERM):
		return true, true
	case errors.Is(err, syscall.ESRCH):
		return false, true
	default:
		return false, false
	}
}
