// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

//go:build windows

package main

import "syscall"

// detachAttr on Windows uses a new process group so the spawned background process is
// not tied to the parent's console (Setpgid is Unix-only).
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x00000200} // CREATE_NEW_PROCESS_GROUP
}
