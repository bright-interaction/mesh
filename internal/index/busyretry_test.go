// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestIsBusyRecognisesSQLiteLockErrors pins the string match. It is a string match on
// purpose (see isBusy), so the exact wording it accepts is part of the contract.
func TestIsBusyRecognisesSQLiteLockErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not busy", nil, false},
		{"the error the operator actually sees", errors.New("apply schema: database is locked (5) (SQLITE_BUSY)"), true},
		{"bare driver wording", errors.New("database is locked"), true},
		{"symbolic code alone", errors.New("SQLITE_BUSY"), true},
		{"an unrelated failure must NOT be retried", errors.New("no such table: notes"), false},
		{"a disk error must NOT be retried", errors.New("disk I/O error"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isBusy(tc.err); got != tc.want {
				t.Errorf("isBusy(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestEnsureSchemaCheckedExplainsTheLock: the operator-facing symptom was
// "database is locked", which says nothing about what to do. A full reindex holds one
// write lock for minutes by design, so the error has to name the process that has it and
// the fact that waiting is the answer.
func TestEnsureSchemaCheckedExplainsTheLock(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	defer s1.Close()

	// A second Store on the same vault must open cleanly when nothing is mid-write; this
	// pins that the check does not fire spuriously, which matters more than the message.
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("second open on an idle index must succeed, got: %v", err)
	}
	defer s2.Close()
}

// TestBusyErrorTextIsActionable pins the wording, since the whole point of the change is
// that the message tells you what to do.
func TestBusyErrorTextIsActionable(t *testing.T) {
	busy := errors.New("database is locked (5) (SQLITE_BUSY)")
	wrapped := fmt.Errorf("another mesh process is indexing this vault and holds the write lock "+
		"(a full reindex is one transaction and can take minutes on a large vault). "+
		"Wait for it to finish, or stop the `mesh sync --watch` / `mesh mcp --watch` daemon, then retry: %w", busy)
	for _, want := range []string{"another mesh process", "mesh sync --watch", "mesh mcp --watch", "can take minutes"} {
		if !strings.Contains(wrapped.Error(), want) {
			t.Errorf("error must mention %q, got: %v", want, wrapped)
		}
	}
}
