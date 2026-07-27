// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
)

// TestWalPinnedByLeakedRows proves the mechanism behind the SQLITE_BUSY that operators
// hit on `mesh index`, and pins the fix.
//
// In WAL mode an open *sql.Rows holds a READ SNAPSHOT. A checkpoint can only reclaim WAL
// frames older than the oldest live snapshot, so one leaked Rows stops the WAL shrinking
// for the life of the process. In a long-running daemon (`mesh sync --watch`,
// `mesh mcp --watch`) the WAL then grows without bound and other processes' writes get
// starved into SQLITE_BUSY.
//
// Observed in production: a sync daemon up 32 hours held the write lock ~100% of the time
// (a 45s probe got 0 acquisitions out of 276 attempts) with a 24MB WAL, and `mesh index`
// failed 8 of 8 runs. Restarting that daemon took it to 0% busy and 0 failures. Seven call
// sites in this package returned early from inside a `for rows.Next()` loop without
// closing, which is what accumulated the leaks.
func TestWalPinnedByLeakedRows(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	dbp := dir + "/.mesh/mesh.db"

	write := func(n int) {
		for i := 0; i < n; i++ {
			_ = s.Write(func(tx *sql.Tx) error {
				_, e := tx.Exec(`INSERT OR REPLACE INTO meta(key,value) VALUES(?,?)`,
					fmt.Sprintf("k%d", i), fmt.Sprintf("%0500d", i))
				return e
			})
		}
	}
	walMB := func() float64 {
		fi, err := os.Stat(dbp + "-wal")
		if err != nil {
			return 0
		}
		return float64(fi.Size()) / 1024 / 1024
	}

	write(400)
	_, _ = s.writeDB.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	t.Logf("baseline WAL after checkpoint: %.2f MB", walMB())

	// Leak a Rows exactly the way an early return inside a rows loop does.
	reader, err := sql.Open("sqlite", dsn(dbp))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	leaked, err := reader.Query(`SELECT key,value FROM meta`)
	if err != nil {
		t.Fatal(err)
	}
	leaked.Next() // start the read txn, then abandon it (the leak)

	write(400)
	_, _ = s.writeDB.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	pinned := walMB()
	t.Logf("WAL with a LEAKED rows held:   %.2f MB  (checkpoint could not reclaim)", pinned)

	leaked.Close() // exactly what the added `defer rows.Close()` does
	write(10)
	_, _ = s.writeDB.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	freed := walMB()
	t.Logf("WAL after closing the rows:    %.2f MB  (checkpoint reclaimed)", freed)

	if pinned <= freed {
		t.Errorf("expected the leaked rows to pin the WAL: pinned=%.2f freed=%.2f", pinned, freed)
	}
}
