// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"database/sql"
	"fmt"
	"testing"
)

// probeCheckpoint runs one TRUNCATE checkpoint from a fresh connection with almost no
// patience and returns the three columns SQLite reports: busy, log (WAL frames), and
// checkpointed. A short busy_timeout matters: with the DSN's 30s the blocked cases below
// would each stall the test for half a minute.
func probeCheckpoint(t *testing.T, dbPath string) (busy, logFrames, checkpointed int) {
	t.Helper()
	c, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=busy_timeout(200)", dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
		t.Fatalf("wal_checkpoint: %v", err)
	}
	return busy, logFrames, checkpointed
}

// TestBlockedCheckpointTellsReaderFromWriter is the triage test for "the WAL will not
// shrink". It pins which holder produces which `PRAGMA wal_checkpoint(TRUNCATE)` result
// row, so the next operator reads the answer off the row instead of guessing.
//
// This exists because the guess was wrong once and cost a full investigation. The live
// Corpus vault reported `busy|log|checkpointed = 1|0|0` with an 11.5MB WAL that kept
// growing, and that was attributed to a leaked *sql.Rows pinning a read snapshot (the
// 2026-07-27 defect shape). It cannot be: a pinned read snapshot leaves the frames it
// pins IN the WAL, so log is always > 0. log == 0 means the WAL holds no frames at all
// and the checkpoint still could not reset the file, which is the WRITE lock.
//
//	leaked read snapshot, frames committed after it   -> busy=1, log>0, checkpointed=0
//	leaked read snapshot at the WAL head              -> busy=1, log>0, checkpointed=log
//	another connection holding an open write txn      -> busy=1, log=0, checkpointed=0
//
// So `1|0|0` says "someone is mid-write", not "someone leaked a Rows". On this estate
// that someone is a sibling mesh watcher inside its reconcile transaction; see
// TestOneChangedNoteRewritesTheWholeGraph for why one edited note is a whole-vault write.
func TestBlockedCheckpointTellsReaderFromWriter(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	dbPath := s.Path()

	fill := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			if err := s.Write(func(tx *sql.Tx) error {
				_, e := tx.Exec(`INSERT OR REPLACE INTO meta(key,value) VALUES(?,?)`,
					fmt.Sprintf("k%d", i), fmt.Sprintf("%0900d", i))
				return e
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	fill(300)
	if b, l, _ := probeCheckpoint(t, dbPath); b != 0 || l != 0 {
		t.Fatalf("baseline should checkpoint cleanly, got busy=%d log=%d", b, l)
	}

	// A leaked read snapshot, with frames committed after it was taken.
	reader, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	leaked, err := reader.Query(`SELECT key, value FROM meta`)
	if err != nil {
		t.Fatal(err)
	}
	leaked.Next() // opens the read transaction, then we abandon it
	fill(300)
	busy, logFrames, _ := probeCheckpoint(t, dbPath)
	if busy != 1 {
		t.Errorf("a leaked read snapshot must block a TRUNCATE checkpoint, got busy=%d", busy)
	}
	if logFrames == 0 {
		t.Errorf("a leaked read snapshot must leave frames IN the WAL (log>0), got log=%d; "+
			"if a blocked checkpoint ever reports log=0 the holder is a WRITER, not a reader", logFrames)
	}
	leaked.Close()
	reader.Close()

	// A leaked read snapshot taken AFTER every frame was committed: the checkpoint can
	// copy every frame but still cannot reset the file. log is still non-zero.
	fill(300)
	r2, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	leaked2, err := r2.Query(`SELECT key, value FROM meta`)
	if err != nil {
		t.Fatal(err)
	}
	leaked2.Next()
	busy, logFrames, checkpointed := probeCheckpoint(t, dbPath)
	if busy != 1 || logFrames == 0 || checkpointed != logFrames {
		t.Errorf("read snapshot at the WAL head: want busy=1 log>0 checkpointed==log, got %d|%d|%d",
			busy, logFrames, checkpointed)
	}
	leaked2.Close()
	r2.Close()
	if b, _, _ := probeCheckpoint(t, dbPath); b != 0 {
		t.Fatalf("closing the leaked rows must release the snapshot, got busy=%d", b)
	}

	// An open WRITE transaction on another connection, nothing committed: this is the
	// only holder that yields log=0, which is the row the live vault reported.
	other, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	tx, err := other.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO meta(key,value) VALUES('writer','held')`); err != nil {
		t.Fatal(err)
	}
	busy, logFrames, checkpointed = probeCheckpoint(t, dbPath)
	if busy != 1 || logFrames != 0 || checkpointed != 0 {
		t.Errorf("open write txn: want the live signature busy=1 log=0 checkpointed=0, got %d|%d|%d",
			busy, logFrames, checkpointed)
	}
	_ = tx.Rollback()
	if b, _, _ := probeCheckpoint(t, dbPath); b != 0 {
		t.Fatalf("rolling back the write txn must unblock the checkpoint, got busy=%d", b)
	}
}
