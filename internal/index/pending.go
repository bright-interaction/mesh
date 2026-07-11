// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"
	"time"
)

// PendingNote is an auto-extracted write-back candidate awaiting human review. It
// mirrors the mesh_append_note fields so a promoted candidate becomes a note with no
// remapping. It is NOT in retrieval until promoted.
type PendingNote struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Title      string `json:"title"`
	Do         string `json:"do"`
	Dont       string `json:"dont"`
	Why        string `json:"why"`
	Confidence string `json:"confidence"`
	Source     string `json:"source"`
	CreatedAt  int64  `json:"created_at"`
}

// PendingID derives a stable id from type+title so re-extracting the same session (or
// the same learning from two sessions) does not create duplicate review items.
func PendingID(noteType, title string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(noteType) + "|" + strings.TrimSpace(title))))
	return "pending-" + hex.EncodeToString(sum[:8])
}

// pendingQueueCap bounds the review queue. Extraction is non-deterministic and the
// BYOAI can reword the same learning across runs, so despite the dedup the queue could
// grow without limit and bury the reviewer. When the cap is exceeded the OLDEST items
// are dropped (a reviewer works newest-first; a genuinely important old learning gets
// re-extracted the next time a session touches it).
const pendingQueueCap = 200

// AddPending stores a candidate for review. Idempotent on (type,title): a duplicate
// extraction updates the existing row rather than piling up review items. The queue is
// capped at pendingQueueCap; older items beyond the cap are pruned in the same tx.
func (s *Store) AddPending(p PendingNote) error {
	if strings.TrimSpace(p.Title) == "" || strings.TrimSpace(p.Type) == "" {
		return nil
	}
	if p.ID == "" {
		p.ID = PendingID(p.Type, p.Title)
	}
	if p.CreatedAt == 0 {
		p.CreatedAt = time.Now().Unix()
	}
	return s.Write(func(tx *sql.Tx) error {
		if _, err := tx.Exec(
			`INSERT INTO pending_notes(id,type,title,do_text,dont_text,why,confidence,source,created_at)
			 VALUES(?,?,?,?,?,?,?,?,?)
			 ON CONFLICT(id) DO UPDATE SET
			   do_text=excluded.do_text, dont_text=excluded.dont_text, why=excluded.why,
			   confidence=excluded.confidence, source=excluded.source`,
			p.ID, p.Type, p.Title, p.Do, p.Dont, p.Why, p.Confidence, p.Source, p.CreatedAt); err != nil {
			return err
		}
		// Cap the queue: keep only the newest pendingQueueCap rows.
		_, err := tx.Exec(
			`DELETE FROM pending_notes WHERE id NOT IN (
			   SELECT id FROM pending_notes ORDER BY created_at DESC, id DESC LIMIT ?)`,
			pendingQueueCap)
		return err
	})
}

// ListPending returns review items, newest first.
func (s *Store) ListPending() ([]PendingNote, error) {
	rows, err := s.readDB.Query(
		`SELECT id,type,title,COALESCE(do_text,''),COALESCE(dont_text,''),COALESCE(why,''),
		        COALESCE(confidence,''),COALESCE(source,''),created_at
		   FROM pending_notes ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingNote
	for rows.Next() {
		var p PendingNote
		if err := rows.Scan(&p.ID, &p.Type, &p.Title, &p.Do, &p.Dont, &p.Why, &p.Confidence, &p.Source, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPending fetches one review item by id.
func (s *Store) GetPending(id string) (PendingNote, error) {
	var p PendingNote
	err := s.readDB.QueryRow(
		`SELECT id,type,title,COALESCE(do_text,''),COALESCE(dont_text,''),COALESCE(why,''),
		        COALESCE(confidence,''),COALESCE(source,''),created_at
		   FROM pending_notes WHERE id=?`, id).
		Scan(&p.ID, &p.Type, &p.Title, &p.Do, &p.Dont, &p.Why, &p.Confidence, &p.Source, &p.CreatedAt)
	return p, err
}

// DeletePending removes a review item (on promote or discard).
func (s *Store) DeletePending(id string) error {
	return s.Write(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM pending_notes WHERE id=?`, id)
		return err
	})
}

// PendingCount returns the number of review items (for the dashboard badge).
func (s *Store) PendingCount() (int, error) {
	var n int
	err := s.readDB.QueryRow(`SELECT count(*) FROM pending_notes`).Scan(&n)
	return n, err
}
