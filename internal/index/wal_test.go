// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// A clean shutdown must reclaim the WAL to zero, not leave a 16MB high-water-mark file
// for the next process to inherit and re-grow from. journal_size_limit only caps the
// WAL; the TRUNCATE checkpoint zeros it.
func TestCheckpointTruncateReclaimsWAL(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	walPath := filepath.Join(dir, ".mesh", "mesh.db-wal")

	// Many small transactions grow the WAL without tripping the default autocheckpoint.
	for i := 0; i < 100; i++ {
		if err := s.RecordWriteback(fmt.Sprintf("note-%d", i), "agent"); err != nil {
			t.Fatal(err)
		}
	}
	fi, err := os.Stat(walPath)
	if err != nil || fi.Size() == 0 {
		t.Skip("WAL not grown before checkpoint (autocheckpoint ran); size assertion not meaningful here")
	}

	s.checkpointTruncateBestEffort()

	fi, err = os.Stat(walPath)
	if err != nil {
		return // file removed = fully reclaimed, also a pass
	}
	if fi.Size() != 0 {
		t.Errorf("TRUNCATE checkpoint should reclaim the WAL to 0 bytes, still %d bytes", fi.Size())
	}
}
