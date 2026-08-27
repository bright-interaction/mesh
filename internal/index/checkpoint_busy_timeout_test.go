// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"testing"
)

// TestTruncateCheckpointKeepsTheWriterBusyTimeout pins the one place a PRAGMA leaks out
// of the statement that set it.
//
// writeDB is capped at ONE connection, so any `PRAGMA busy_timeout` executed on it is
// process-wide and permanent. The pool now deliberately keeps a short slice so context
// cancellation is observed between BeginTx attempts; runTxContext supplies the aggregate
// 30-second patience for legacy Background writes. checkpointTruncateBestEffort uses its
// own connection and must not change that slice in either direction.
func TestTruncateCheckpointKeepsTheWriterBusyTimeout(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	timeout := func() int {
		t.Helper()
		var v int
		if err := s.writeDB.QueryRow("PRAGMA busy_timeout").Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}

	before := timeout()
	if before != writeBusyPollMS {
		t.Fatalf("write pool should keep its cancelable busy slice %d, got %d", writeBusyPollMS, before)
	}
	s.checkpointTruncateBestEffort()
	if after := timeout(); after != before {
		t.Errorf("checkpointTruncateBestEffort left the write pool's busy_timeout at %d instead of %d; "+
			"writeDB has one connection, so this downgrade is permanent and every later "+
			"reconcile now uses the wrong busy slice (delta %dms)", after, before, before-after)
	}
}
