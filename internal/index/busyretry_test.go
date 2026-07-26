// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"errors"
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

// TestRetryOnBusySucceedsAfterTransientLocks is the regression for the operator-facing
// symptom: `mesh index` needing two to four manual attempts because another mesh process
// held the write lock for a moment. The retry is what those manual attempts were doing.
//
// The live race could not be reproduced on demand (12 concurrent `mesh index` runs
// against the real 884-note vault produced zero failures), so this drives the retry
// mechanism directly with a stub instead of hoping to catch the race in CI. It proves
// the loop retries a busy error, stops as soon as one succeeds, and does not retry
// anything else.
func TestRetryOnBusySucceedsAfterTransientLocks(t *testing.T) {
	busy := errors.New("database is locked (5) (SQLITE_BUSY)")
	fatal := errors.New("no such table: notes")

	tests := []struct {
		name      string
		failures  int   // how many busy errors before success
		finalErr  error // what the attempt after those failures returns
		wantCalls int
		wantErr   bool
	}{
		{"succeeds first try", 0, nil, 1, false},
		{"one transient lock", 1, nil, 2, false},
		{"three transient locks (the reported 2-4 attempts)", 3, nil, 4, false},
		{"a non-busy error is returned immediately, not retried", 0, fatal, 1, true},
		{"busy then a real error stops retrying", 1, fatal, 2, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			attempt := func() error {
				calls++
				if calls <= tc.failures {
					return busy
				}
				return tc.finalErr
			}
			err := retryOnBusy(attempt, 8)
			if calls != tc.wantCalls {
				t.Errorf("attempts = %d, want %d", calls, tc.wantCalls)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestRetryOnBusyGivesUpWithAnActionableMessage: a lock held for the whole window is a
// real problem, and the error has to say what to look for rather than repeating the
// bare driver string the operator already could not act on.
func TestRetryOnBusyGivesUpWithAnActionableMessage(t *testing.T) {
	busy := errors.New("database is locked (5) (SQLITE_BUSY)")
	calls := 0
	err := retryOnBusy(func() error { calls++; return busy }, 3)
	if err == nil {
		t.Fatal("a permanently held lock must still fail")
	}
	if calls != 3 {
		t.Errorf("attempts = %d, want 3", calls)
	}
	for _, want := range []string{"mesh sync", "mesh mcp --watch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q so the operator knows what to look for, got: %v", want, err)
		}
	}
}
