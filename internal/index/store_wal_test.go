// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"database/sql"
	"testing"
)

// TestWALJournalSizeLimitApplied is the regression guard for the 223MB-WAL /
// SQLITE_BUSY incident: SQLite's WAL autocheckpoint is passive and never shrinks
// mesh.db-wal, and nothing issued a TRUNCATE, so the WAL grew without bound. The fix
// sets journal_size_limit on the DSN (so every checkpoint truncates the WAL) and adds
// a periodic PASSIVE checkpoint on the writer goroutine. This asserts the DSN pragma
// is live on both pools, and that a write + Close still work with the ticker running.
func TestWALJournalSizeLimitApplied(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	for name, db := range map[string]*sql.DB{"writeDB": s.writeDB, "readDB": s.readDB} {
		var limit int64
		if err := db.QueryRow("PRAGMA journal_size_limit").Scan(&limit); err != nil {
			t.Fatalf("%s: query journal_size_limit: %v", name, err)
		}
		if limit != walSizeLimit {
			t.Errorf("%s: journal_size_limit = %d, want %d", name, limit, walSizeLimit)
		}
	}

	// A write still succeeds with the periodic-checkpoint ticker in the writer's
	// select, and the deferred Close cleanly joins the writer despite the new case.
	if err := s.Write(func(tx *sql.Tx) error {
		_, e := tx.Exec("INSERT OR REPLACE INTO meta(key, value) VALUES('wal_test', '1')")
		return e
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
}
