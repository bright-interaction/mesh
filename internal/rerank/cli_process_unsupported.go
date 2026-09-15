//go:build !linux && !darwin

// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package rerank

import (
	"fmt"
	"os/exec"
	"runtime"
)

func cliProcessSupported() error {
	return fmt.Errorf("subscription CLI process supervision is unsupported on %s", runtime.GOOS)
}

func configureCLIProcess(*exec.Cmd)  {}
func stopCLIProcess(*exec.Cmd) error { return cliProcessSupported() }
