// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
)

// runRoot executes the real root command with args and returns everything it wrote.
// Going through rootCmd() (not versionCmd() directly) is the point: the regression
// being guarded is that `mesh version` and `mesh --version` were BOTH unwired, so the
// test has to prove they are reachable from the command tree a user actually runs.
func runRoot(t *testing.T, args ...string) string {
	t.Helper()
	var buf bytes.Buffer
	root := rootCmd()
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("mesh %s: %v (output %q)", strings.Join(args, " "), err, buf.String())
	}
	return buf.String()
}

func TestVersionCommand(t *testing.T) {
	out := strings.TrimSpace(runRoot(t, "version"))
	if !strings.HasPrefix(out, "mesh ") {
		t.Errorf("version output should name the program, got %q", out)
	}
	// The build stamp: unstamped local builds report "dev", the Makefile and Dockerfile
	// replace it with the commit via -ldflags. Either way it must not be empty.
	if !strings.Contains(out, buildVersionForTest()) {
		t.Errorf("version output %q should contain the build version %q", out, buildVersionForTest())
	}
	// SECURITY.md reporters need the toolchain too (a bug can be toolchain specific).
	if !strings.Contains(out, runtime.Version()) {
		t.Errorf("version output %q should contain the Go runtime %q", out, runtime.Version())
	}
}

// TestVersionFlagMatchesCommand pins the two surfaces together: cobra's built-in
// --version flag renders from its own template, so without SetVersionTemplate it would
// silently print a different string than `mesh version`.
func TestVersionFlagMatchesCommand(t *testing.T) {
	cmdOut := strings.TrimSpace(runRoot(t, "version"))
	flagOut := strings.TrimSpace(runRoot(t, "--version"))
	if cmdOut != flagOut {
		t.Errorf("`mesh version` printed %q but `mesh --version` printed %q", cmdOut, flagOut)
	}
}

func buildVersionForTest() string {
	// versionLine() is the only formatter; recompute its version half the same way so
	// the assertion tracks a stamped build instead of hardcoding "dev".
	line := versionLine()
	open := strings.LastIndex(line, " (")
	if open < 0 {
		return line
	}
	return strings.TrimPrefix(line[:open], "mesh ")
}
