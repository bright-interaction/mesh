// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/bright-interaction/mesh/internal/latency"
)

const healthAnalysisBound = 2 * time.Minute
const healthPublishBound = 2 * time.Second
const healthRetryInterval = 30 * time.Second

var errHealthChanged = errors.New("health inputs changed during analysis")

// scheduleBackgroundHealth coalesces ticks, including across LiveIndexers sharing
// a Store. It never waits for analysis or database work. run is a per-call test
// seam; production always uses refreshBackgroundHealth.
func (s *Store) scheduleBackgroundHealth(root string, run func(context.Context) error) {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if s.readOnly || s.healthClosed || s.healthCancel != nil || time.Since(s.healthLastAttempt) < healthRefreshInterval {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), healthAnalysisBound)
	done := make(chan struct{})
	s.healthCancel, s.healthDone, s.healthLastAttempt = cancel, done, time.Now()
	if run == nil {
		run = func(ctx context.Context) error { return s.refreshBackgroundHealth(ctx, root) }
	}
	go func() {
		err := run(ctx)
		cancel()
		if err != nil {
			// No raw errors: filesystem paths and note content do not belong in
			// these diagnostics. Prior report remains intact on every failure.
			slog.Info("mesh: background health retained previous report", "inputs_changed", errors.Is(err, errHealthChanged), "canceled", errors.Is(err, context.Canceled), "deadline", errors.Is(err, context.DeadlineExceeded))
		}
		s.healthMu.Lock()
		if err != nil {
			s.healthLastAttempt = time.Now().Add(-healthRefreshInterval + healthRetryInterval)
		}
		s.healthCancel = nil
		close(done)
		s.healthMu.Unlock()
	}()
}

// Close seals scheduling before cancellation and joins the pass before closing
// either database pool. A late analysis can neither publish nor outlive the Store.
func (s *Store) stopBackgroundHealth() {
	s.healthMu.Lock()
	s.healthClosed = true
	if s.healthCancel != nil {
		s.healthCancel()
	}
	done := s.healthDone
	s.healthMu.Unlock()
	if done != nil {
		<-done
	}
}

func (s *Store) refreshBackgroundHealth(ctx context.Context, root string) error {
	return s.refreshBackgroundHealthWith(ctx, root, nil)
}

// afterScan is a test seam for races at the analysis/publication boundary.
func (s *Store) refreshBackgroundHealthWith(ctx context.Context, root string, afterScan func()) error {
	trace := latency.Start("background_health", "snapshot")
	defer trace.End()
	// Capture the indexed input identity in one read transaction, then RELEASE
	// it before slow filesystem/Git analysis so it cannot pin the WAL for minutes.
	tx, err := s.readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	before, err := healthInputDigest(ctx, tx)
	_ = tx.Rollback()
	if err != nil {
		return err
	}
	now := time.Now()
	trace.Phase("scan_health")
	findings, err := s.scanHealthContext(ctx, root, now)
	if err != nil {
		return err
	}
	trace.Phase("contradictions")
	contradictions, err := s.scanContradictionsContext(ctx)
	if err != nil {
		return err
	}
	findings = append(findings, contradictions...)
	if afterScan != nil {
		afterScan()
	}
	trace.Phase("publish")
	// Include queueing, validation and publication in the small cooperative budget.
	// The shared writer's ownership-metadata lease and kernel commit calls are not
	// forcibly interruptible; join admitted work rather than abandon a live write.
	// Never hold the writer while walking files, running Git or comparing guidance.
	pubCtx, cancel := context.WithTimeout(ctx, healthPublishBound)
	defer cancel()
	err = s.WriteContext(pubCtx, func(tx *sql.Tx) error {
		after, err := healthInputDigest(pubCtx, tx)
		if err != nil {
			return err
		}
		if after != before {
			return errHealthChanged
		}
		if _, err := tx.ExecContext(pubCtx, `DELETE FROM note_health WHERE issue IN ('dead_ref','overdue','contradiction')`); err != nil {
			return err
		}
		for _, f := range findings {
			if _, err := tx.ExecContext(pubCtx, `INSERT INTO note_health(note_id,path,issue,detail,detected_at) VALUES(?,?,?,?,?)`, f.NoteID, f.Path, f.Issue, f.Detail, now.Unix()); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(pubCtx, `INSERT OR REPLACE INTO meta(key,value) VALUES('health_completed_at', ?)`, time.Now().UTC().Format(time.RFC3339))
		return err
	})
	if err == nil {
		slog.Info("mesh: background health published", "findings", len(findings))
	}
	return err
}

// Identity includes ALL indexed health inputs, not just findings: a new note can
// invalidate a contradiction result. Validate again INSIDE the write transaction
// so an edit cannot slip between the check and publication. External working-tree
// checks remain best-effort observations, just as with explicit mesh health.
func healthInputDigest(ctx context.Context, tx *sql.Tx) ([32]byte, error) {
	h := sha256.New()
	enc := json.NewEncoder(h)
	for _, query := range []string{
		`SELECT id,path,retrieval_hash,frontmatter,COALESCE(review_by,'') FROM notes ORDER BY id`,
		`SELECT path FROM code_files ORDER BY path`,
		`SELECT value FROM meta WHERE key='code_roots'`,
	} {
		if err := digestHealthQuery(ctx, tx, query, enc); err != nil {
			return [32]byte{}, err
		}
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest, ctx.Err()
}

func digestHealthQuery(ctx context.Context, tx *sql.Tx, query string, enc *json.Encoder) error {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	_ = enc.Encode(query)
	for rows.Next() {
		values := make([]string, len(cols))
		args := make([]any, len(cols))
		for i := range args {
			args[i] = &values[i]
		}
		if err := rows.Scan(args...); err != nil {
			return err
		}
		_ = enc.Encode(values)
	}
	return rows.Err()
}
