//go:build linux || darwin

// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package rerank

import (
	"errors"
	"os/exec"
	"syscall"
)

func cliProcessSupported() error { return nil }

func configureCLIProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return stopCLIProcess(cmd) }
}

// Signal only the group created for this command. This contains ordinary CLI
// descendants, not adversarial children that deliberately create new sessions.
// Cmd.Wait reaps the direct child and bounds/joins its IO workers via WaitDelay;
// the OS reaps orphaned grandchildren. This is not a security sandbox.
func stopCLIProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
