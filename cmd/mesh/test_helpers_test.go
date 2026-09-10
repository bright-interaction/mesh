// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"io"
	"os"
	"testing"
)

// captureStdout lives in a hub-free test file because both open-core and
// hub-harness tests use it. The public mirror strips conflicts_test.go.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := fn()
	_ = w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	return string(b), runErr
}
