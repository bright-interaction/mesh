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
// process-wide and permanent: the connection goes back to the pool carrying the new value
// and every later write transaction inherits it. checkpointTruncateBestEffort wants a
// short timeout for itself (2s) so a contended WAL degrades to "not this tick" instead of
// stalling the writer, and it used to get that by setting the PRAGMA on the write pool.
// The cost was the daemon's REAL writes: after the first WAL-over-16MB tick they gave up
// after 2 seconds instead of the DSN's 30, so under sibling-watcher contention a reconcile
// returned SQLITE_BUSY, the drift stayed, and the next tick failed the same way. The
// trigger is a large WAL, which is itself a symptom of contention, so the downgrade
// arrives exactly when the extra patience is most needed.
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
	if before != busyTimeoutMS {
		t.Fatalf("write pool should start at the DSN's busy_timeout %d, got %d", busyTimeoutMS, before)
	}
	s.checkpointTruncateBestEffort()
	if after := timeout(); after != before {
		t.Errorf("checkpointTruncateBestEffort left the write pool's busy_timeout at %d instead of %d; "+
			"writeDB has one connection, so this downgrade is permanent and every later "+
			"reconcile now fails SQLITE_BUSY %dms sooner", after, before, before-after)
	}
}
