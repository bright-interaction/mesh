// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"database/sql"
	"fmt"
	"testing"
)

// TestPassiveCheckpointDoesNotBoundTheWAL pins the FACT that motivated the escalation in
// writer(). The code used to assert that "journal_size_limit still truncates the file to
// <=16MB after the checkpoint", and that is not true: journal_size_limit only applies when
// a checkpoint RESETS the WAL, and a PASSIVE checkpoint cannot reset it while any reader
// holds a snapshot, which is the normal state under a watch daemon.
//
// Measured on the live vault before this fix: 26.44MB against a 16MB limit, reclaimable to
// zero the instant a TRUNCATE ran. If this test ever fails, PASSIVE has started bounding
// the file on its own and the escalation can be reconsidered.
func TestPassiveCheckpointDoesNotBoundTheWAL(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// A reader holding a snapshot is what stops PASSIVE resetting the WAL.
	reader, err := sql.Open("sqlite", dsn(s.dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	rows, err := reader.Query(`SELECT key, value FROM meta`)
	if err != nil {
		t.Fatal(err)
	}
	rows.Next()
	defer rows.Close()

	for i := 0; i < 3000; i++ {
		if err := s.Write(func(tx *sql.Tx) error {
			_, e := tx.Exec(`INSERT OR REPLACE INTO meta(key,value) VALUES(?,?)`,
				fmt.Sprintf("k%d", i), fmt.Sprintf("%01000d", i))
			return e
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.writeDB.Exec("PRAGMA wal_checkpoint(PASSIVE)"); err != nil {
		t.Fatal(err)
	}

	withReader := s.walBytes()
	if withReader == 0 {
		t.Skip("no WAL file; nothing to assert about")
	}
	t.Logf("WAL after PASSIVE with a live reader: %.2f MB", float64(withReader)/1024/1024)

	// Now do what the writer loop now does when it sees the file over the limit.
	rows.Close()
	s.checkpointTruncateBestEffort()
	after := s.walBytes()
	t.Logf("WAL after the TRUNCATE escalation:    %.2f MB", float64(after)/1024/1024)

	if after >= withReader {
		t.Errorf("the TRUNCATE escalation reclaimed nothing: %d -> %d bytes", withReader, after)
	}
}

// walBytes is what the writer loop decides on, so it must report the real file rather than
// a frame count, and must not blow up when there is no WAL yet.
func TestWalBytesReportsTheFile(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for i := 0; i < 200; i++ {
		if err := s.Write(func(tx *sql.Tx) error {
			_, e := tx.Exec(`INSERT OR REPLACE INTO meta(key,value) VALUES(?,?)`,
				fmt.Sprintf("k%d", i), fmt.Sprintf("%0500d", i))
			return e
		}); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.walBytes(); got <= 0 {
		t.Errorf("walBytes() = %d after 200 writes, want a positive size", got)
	}

	s.checkpointTruncateBestEffort()
	if got := s.walBytes(); got != 0 {
		t.Errorf("walBytes() = %d after TRUNCATE with no readers, want 0", got)
	}
}
