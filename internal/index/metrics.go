// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"database/sql"
	"encoding/json"
)

// Usage telemetry for the ROI dashboard. Counters are monotonic and local; they
// quantify what Mesh did (queries served, notes fetched/written) so a team can see
// the value, and per-note fetch counts ("fetch:<id>") drive a most-reused list.

// IncrMetric adds n to a counter (upsert). Best-effort at call sites, and deliberately
// NON-BLOCKING: it accumulates in memory and the writer goroutine flushes the batch
// (see Store.drainTelemetry). Every caller is a read path (mesh_search, mesh_fetch, the
// web search API) that has already computed its answer, so it must not inherit the
// latency of whatever transaction the single writer is running, nor the SQLite
// busy_timeout when a second mesh process holds the write lock. Always returns nil; the
// error return is kept because every call site treats it as best-effort already.
func (s *Store) IncrMetric(key string, n int64) error {
	if key == "" {
		return nil
	}
	s.recordTelemetry(key, n, nil)
	return nil
}

// Metric reads one counter (0 if absent).
func (s *Store) Metric(key string) (int64, error) {
	return s.MetricContext(context.Background(), key)
}

// MetricContext is Metric with caller-controlled cancellation.
func (s *Store) MetricContext(ctx context.Context, key string) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Reporting surface, not a hot path: land pending increments first so a dashboard
	// never shows a number that lags the flush ticker.
	s.flushReportingTelemetryContext(ctx)
	var v int64
	err := s.readDB.QueryRowContext(ctx, `SELECT value FROM metrics WHERE key=?`, key).Scan(&v)
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
	return s.TopFetchedContext(context.Background(), limit)
}

// TopFetchedContext is TopFetched with caller-controlled cancellation.
func (s *Store) TopFetchedContext(ctx context.Context, limit int) ([]struct {
	NoteID string `json:"note_id"`
	Count  int64  `json:"count"`
}, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if limit <= 0 {
		limit = 10
	}
	s.flushReportingTelemetryContext(ctx) // reporting surface: include increments not yet flushed
	rows, err := s.readDB.QueryContext(ctx,
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
	return s.NotesByTypeContext(context.Background())
}

// NotesByTypeContext is NotesByType with caller-controlled cancellation.
func (s *Store) NotesByTypeContext(ctx context.Context) (map[string]int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := s.readDB.QueryContext(ctx, `SELECT type, count(*) FROM notes GROUP BY type`)
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
	return s.ContributorCountsContext(context.Background())
}

// ContributorCountsContext is ContributorCounts with caller-controlled cancellation.
func (s *Store) ContributorCountsContext(ctx context.Context) (map[string]int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := s.readDB.QueryContext(ctx, `SELECT frontmatter FROM notes`)
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

// flushReportingTelemetryContext keeps the reporting surfaces' historical one-second
// ceiling while allowing a shorter caller deadline (including request cancellation) to
// win. Telemetry is best-effort and must never inherit SQLite's normal 30-second write
// patience merely because the public Context API was given context.Background.
func (s *Store) flushReportingTelemetryContext(ctx context.Context) {
	flushCtx, cancel := context.WithTimeout(ctx, telemetryFlushTimeout)
	defer cancel()
	s.flushTelemetryContext(flushCtx)
}
