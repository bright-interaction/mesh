// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"encoding/json"
	"strings"
)

// GotchaRow is a gotcha's guidance, for turning institutional rules into enforcement
// (the gotcha->guard feature). Only gotchas with a concrete "dont" (an anti-pattern to
// detect) are candidates; judgment-only notes are not mechanically checkable.
type GotchaRow struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Do         string `json:"do"`
	Dont       string `json:"dont"`
	Why        string `json:"why"`
	Confidence string `json:"confidence"`
}

// Gotchas returns gotcha notes that have an anti-pattern (non-empty dont). When
// highOnly is set, only confidence=high ones (the safest to turn into a guard).
func (s *Store) Gotchas(highOnly bool) ([]GotchaRow, error) {
	rows, err := s.readDB.Query(`SELECT id, title, frontmatter FROM notes WHERE type = 'gotcha' ORDER BY title`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GotchaRow
	for rows.Next() {
		var id, title, fmJSON string
		if err := rows.Scan(&id, &title, &fmJSON); err != nil {
			return nil, err
		}
		var fm struct {
			Do, Dont, Why, Confidence string
		}
		_ = json.Unmarshal([]byte(fmJSON), &fm)
		if strings.TrimSpace(fm.Dont) == "" {
			continue // no anti-pattern to enforce
		}
		if highOnly && strings.ToLower(strings.TrimSpace(fm.Confidence)) != "high" {
			continue
		}
		out = append(out, GotchaRow{ID: id, Title: title, Do: fm.Do, Dont: fm.Dont, Why: fm.Why, Confidence: fm.Confidence})
	}
	return out, rows.Err()
}
