// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"database/sql"
	"sort"
	"time"
)

// The flywheel measurement: prove (not assert) that agent write-backs get reused by a
// LATER session. RecordWriteback stamps each write-back at authoring time;
// RecordReuse counts a fetch as reuse only if it lands at least gapSec after authoring
// (the cross-session proxy: a re-read inside the same work burst is not "the next run
// inheriting it"). FlywheelStats turns the table into the three numbers that say
// whether the moat is real: reuse rate, time-to-reuse, and write-back input health.

// RecordWriteback stamps a note as a write-back at authoring time so its reuse can be
// measured. Idempotent: re-writing the same id keeps the original authored_at (the
// flywheel cares about when the knowledge first existed). Best-effort at call sites.
func (s *Store) RecordWriteback(noteID, source string) error {
	if noteID == "" {
		return nil
	}
	now := time.Now().Unix()
	return s.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO note_reuse(note_id, authored_at, source) VALUES(?,?,?)
			 ON CONFLICT(note_id) DO NOTHING`,
			noteID, now, source)
		return err
	})
}

// BackfillWritebacks seeds note_reuse from existing agent-authored notes, so the
// flywheel measurement reflects the whole accumulated corpus on day one instead of
// only notes written after this shipped. Idempotent (ON CONFLICT DO NOTHING); uses the
// note's mtime as authoring time, which makes any later fetch of an old note count as
// genuine cross-session reuse. Returns the number of notes newly tracked.
func (s *Store) BackfillWritebacks() (int, error) {
	var n int
	err := s.Write(func(tx *sql.Tx) error {
		res, e := tx.Exec(
			`INSERT INTO note_reuse(note_id, authored_at, source)
			   SELECT id, mtime, source FROM notes WHERE source = 'agent'
			 ON CONFLICT(note_id) DO NOTHING`)
		if e != nil {
			return e
		}
		ra, _ := res.RowsAffected()
		n = int(ra)
		return nil
	})
	return n, err
}

// RecordReuse records a fetch of a tracked note as reuse, but only when it happens at
// least gapSec after the note was authored. A no-op for untracked notes (those not
// written back through Mesh) and for fetches inside the authoring burst. Best-effort.
func (s *Store) RecordReuse(noteID string, gapSec int64) error {
	if noteID == "" {
		return nil
	}
	now := time.Now().Unix()
	return s.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`UPDATE note_reuse
			    SET reuse_count = reuse_count + 1,
			        first_reuse = COALESCE(first_reuse, ?),
			        last_reuse  = ?
			  WHERE note_id = ? AND (? - authored_at) >= ?`,
			now, now, noteID, now, gapSec)
		return err
	})
}

// FlywheelStats is the headline answer to "does the flywheel compound?".
type FlywheelStats struct {
	Authored           int64   `json:"authored"`              // write-backs being tracked
	Reused             int64   `json:"reused"`                // how many got >=1 cross-session reuse
	ReuseRatePct       float64 `json:"reuse_rate_pct"`        // reused / authored
	TotalReuses        int64   `json:"total_reuses"`          // sum of qualifying fetches
	MedianHoursToReuse float64 `json:"median_hours_to_reuse"` // over reused notes (0 if none)
	WritesPer100Reads  float64 `json:"writes_per_100_reads"`  // input-health proxy (writes / (queries+fetches))
}

// FlywheelStats computes the reuse picture from note_reuse plus the usage counters.
func (s *Store) FlywheelStats() (FlywheelStats, error) {
	var st FlywheelStats
	if err := s.readDB.QueryRow(
		`SELECT count(*),
		        count(first_reuse),
		        COALESCE(sum(reuse_count),0)
		   FROM note_reuse`).Scan(&st.Authored, &st.Reused, &st.TotalReuses); err != nil {
		return st, err
	}
	if st.Authored > 0 {
		st.ReuseRatePct = float64(st.Reused) / float64(st.Authored) * 100
	}

	// Median time-to-first-reuse, in hours, over the notes that were reused.
	rows, err := s.readDB.Query(
		`SELECT (first_reuse - authored_at) FROM note_reuse
		  WHERE first_reuse IS NOT NULL AND first_reuse >= authored_at`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	var deltas []int64
	for rows.Next() {
		var d int64
		if err := rows.Scan(&d); err != nil {
			return st, err
		}
		deltas = append(deltas, d)
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	if len(deltas) > 0 {
		sort.Slice(deltas, func(i, j int) bool { return deltas[i] < deltas[j] })
		mid := deltas[len(deltas)/2]
		if len(deltas)%2 == 0 {
			mid = (deltas[len(deltas)/2-1] + deltas[len(deltas)/2]) / 2
		}
		st.MedianHoursToReuse = float64(mid) / 3600
	}

	// Input health: write-backs per 100 reads (queries + fetches). A flywheel with a
	// healthy reuse rate but near-zero input is still starved.
	q, _ := s.Metric("queries")
	f, _ := s.Metric("fetches")
	wr, _ := s.Metric("writes")
	if reads := q + f; reads > 0 {
		st.WritesPer100Reads = float64(wr) / float64(reads) * 100
	}
	return st, nil
}
