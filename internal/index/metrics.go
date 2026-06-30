// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"database/sql"
	"encoding/json"
)

// Usage telemetry for the ROI dashboard. Counters are monotonic and local; they
// quantify what Mesh did (queries served, notes fetched/written) so a team can see
// the value, and per-note fetch counts ("fetch:<id>") drive a most-reused list.

// IncrMetric adds n to a counter (upsert). Best-effort at call sites.
func (s *Store) IncrMetric(key string, n int64) error {
	return s.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			`INSERT INTO metrics(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=value+excluded.value`,
			key, n)
		return err
	})
}

// Metric reads one counter (0 if absent).
func (s *Store) Metric(key string) (int64, error) {
	var v int64
	err := s.readDB.QueryRow(`SELECT value FROM metrics WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return v, err
}

// TopFetched returns the most-fetched notes (id, count), highest first.
func (s *Store) TopFetched(limit int) ([]struct {
	NoteID string `json:"note_id"`
	Count  int64  `json:"count"`
}, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.readDB.Query(
		`SELECT substr(key,7), value FROM metrics WHERE key LIKE 'fetch:%' ORDER BY value DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct {
		NoteID string `json:"note_id"`
		Count  int64  `json:"count"`
	}
	for rows.Next() {
		var r struct {
			NoteID string `json:"note_id"`
			Count  int64  `json:"count"`
		}
		if err := rows.Scan(&r.NoteID, &r.Count); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// NotesByType returns type -> count for coverage.
func (s *Store) NotesByType() (map[string]int, error) {
	rows, err := s.readDB.Query(`SELECT type, count(*) FROM notes GROUP BY type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var t string
		var n int
		if err := rows.Scan(&t, &n); err != nil {
			return nil, err
		}
		out[t] = n
	}
	return out, rows.Err()
}

// ContributorCounts tallies authored notes per author (from provenance frontmatter).
func (s *Store) ContributorCounts() (map[string]int, error) {
	rows, err := s.readDB.Query(`SELECT frontmatter FROM notes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var fmJSON string
		if err := rows.Scan(&fmJSON); err != nil {
			return nil, err
		}
		var fm struct {
			Author string `json:"Author"`
		}
		if json.Unmarshal([]byte(fmJSON), &fm) == nil && fm.Author != "" {
			out[fm.Author]++
		}
	}
	return out, rows.Err()
}
