// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

//go:build windows

package index

// processAlive cannot answer on Windows: os.FindProcess succeeds for any pid and
// Signal(0) is not supported, so a wrong answer here would either wedge a vault whose
// owner has died or steal it from one that has not. Reporting known=false makes the
// caller decide on the lock's heartbeat instead, which is correct on every platform.
func processAlive(pid int) (alive, known bool) { return false, false }
