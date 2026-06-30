// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

//go:build !windows

package main

import "syscall"

// detachAttr puts a spawned background process (the Stop hook's auto-extraction) in
// its own process group so it outlives the short-lived hook that started it.
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
