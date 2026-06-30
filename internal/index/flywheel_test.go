// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"database/sql"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func reuseRow(t *testing.T, s *Store, id string) (authored, reuseCount int64, firstReuse sql.NullInt64) {
	t.Helper()
	err := s.readDB.QueryRow(`SELECT authored_at, reuse_count, first_reuse FROM note_reuse WHERE note_id=?`, id).
		Scan(&authored, &reuseCount, &firstReuse)
	if err != nil {
		t.Fatalf("reuseRow(%s): %v", id, err)
	}
	return
}

func backdate(t *testing.T, s *Store, id string, secsAgo int64) {
	t.Helper()
	if err := s.Write(func(tx *sql.Tx) error {
		_, e := tx.Exec(`UPDATE note_reuse SET authored_at=? WHERE note_id=?`, time.Now().Unix()-secsAgo, id)
		return e
	}); err != nil {
		t.Fatal(err)
	}
}

// A fetch inside the authoring burst is NOT reuse; a fetch after the gap is, and the
// first one stamps time-to-reuse. Untracked notes are a no-op.
func TestRecordReuseGap(t *testing.T) {
	s := openTestStore(t)
	if err := s.RecordWriteback("note-a", "agent"); err != nil {
		t.Fatal(err)
	}

	// Same-burst fetch: below the gap, not counted.
	if err := s.RecordReuse("note-a", 600); err != nil {
		t.Fatal(err)
	}
	if _, rc, fr := reuseRow(t, s, "note-a"); rc != 0 || fr.Valid {
		t.Fatalf("same-burst fetch counted as reuse: count=%d first=%v", rc, fr)
	}

	// Now pretend the note was authored 2h ago: a fetch is cross-session reuse.
	backdate(t, s, "note-a", 7200)
	if err := s.RecordReuse("note-a", 600); err != nil {
		t.Fatal(err)
	}
	_, rc, fr := reuseRow(t, s, "note-a")
	if rc != 1 || !fr.Valid {
		t.Fatalf("post-gap fetch not counted: count=%d first=%v", rc, fr)
	}
	first := fr.Int64
	if err := s.RecordReuse("note-a", 600); err != nil {
		t.Fatal(err)
	}
	_, rc, fr = reuseRow(t, s, "note-a")
	if rc != 2 || fr.Int64 != first {
		t.Fatalf("second reuse: count=%d (want 2), first moved %d->%d", rc, first, fr.Int64)
	}

	// Untracked note: no row created, no error.
	if err := s.RecordReuse("never-written", 600); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = s.readDB.QueryRow(`SELECT count(*) FROM note_reuse WHERE note_id=?`, "never-written").Scan(&n)
	if n != 0 {
		t.Fatalf("untracked note created a reuse row")
	}
}

// Re-writing the same id keeps the original authored_at (knowledge first-existed time).
func TestRecordWritebackIdempotent(t *testing.T) {
	s := openTestStore(t)
	if err := s.RecordWriteback("note-a", "agent"); err != nil {
		t.Fatal(err)
	}
	backdate(t, s, "note-a", 7200)
	authored, _, _ := reuseRow(t, s, "note-a")
	if err := s.RecordWriteback("note-a", "agent"); err != nil {
		t.Fatal(err)
	}
	if a2, _, _ := reuseRow(t, s, "note-a"); a2 != authored {
		t.Fatalf("authored_at changed on re-write: %d -> %d", authored, a2)
	}
}

// BackfillWritebacks seeds note_reuse from existing agent notes only, idempotently.
func TestBackfillWritebacks(t *testing.T) {
	s := openTestStore(t)
	insertNote := func(id, source string) {
		if err := s.Write(func(tx *sql.Tx) error {
			_, e := tx.Exec(
				`INSERT INTO notes(id,path,type,title,retrieval_hash,frontmatter,mtime,source) VALUES(?,?,?,?,?,?,?,?)`,
				id, id+".md", "gotcha", id, "h", "{}", time.Now().Unix()-9999, source)
			return e
		}); err != nil {
			t.Fatal(err)
		}
	}
	insertNote("agent-1", "agent")
	insertNote("manual-1", "manual")

	n, err := s.BackfillWritebacks()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("backfilled %d notes, want 1 (agent only)", n)
	}
	if authored, _, _ := reuseRow(t, s, "agent-1"); authored == 0 {
		t.Fatal("agent note not tracked")
	}
	var m int
	_ = s.readDB.QueryRow(`SELECT count(*) FROM note_reuse WHERE note_id=?`, "manual-1").Scan(&m)
	if m != 0 {
		t.Fatal("manual note must not be tracked as a write-back")
	}
	if n2, _ := s.BackfillWritebacks(); n2 != 0 {
		t.Fatalf("second backfill inserted %d, want 0 (idempotent)", n2)
	}
}

// FlywheelStats reports reuse rate and median time-to-reuse over reused notes.
func TestFlywheelStats(t *testing.T) {
	s := openTestStore(t)
	// note-a authored 2h ago, reused; note-b authored now, never reused.
	for _, id := range []string{"note-a", "note-b"} {
		if err := s.RecordWriteback(id, "agent"); err != nil {
			t.Fatal(err)
		}
	}
	backdate(t, s, "note-a", 7200) // 2h
	if err := s.RecordReuse("note-a", 600); err != nil {
		t.Fatal(err)
	}

	st, err := s.FlywheelStats()
	if err != nil {
		t.Fatal(err)
	}
	if st.Authored != 2 || st.Reused != 1 {
		t.Fatalf("authored=%d reused=%d, want 2/1", st.Authored, st.Reused)
	}
	if st.ReuseRatePct < 49 || st.ReuseRatePct > 51 {
		t.Fatalf("reuse rate = %.1f, want ~50", st.ReuseRatePct)
	}
	if st.MedianHoursToReuse < 1.9 || st.MedianHoursToReuse > 2.1 {
		t.Fatalf("median time-to-reuse = %.2fh, want ~2h", st.MedianHoursToReuse)
	}

	// Input health: 1 write counter vs reads. Record some usage.
	_ = s.IncrMetric("writes", 4)
	_ = s.IncrMetric("queries", 16)
	_ = s.IncrMetric("fetches", 4)
	st, _ = s.FlywheelStats()
	if st.WritesPer100Reads < 19 || st.WritesPer100Reads > 21 {
		t.Fatalf("writes per 100 reads = %.1f, want ~20", st.WritesPer100Reads)
	}
}
