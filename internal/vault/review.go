// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"strings"
	"time"
)

// ReviewOverdue gives health reporting and optional retrieval decay one deadline
// rule. A YYYY-MM-DD review remains due throughout that UTC calendar day. An
// RFC3339 timestamp is overdue once its instant has passed, respecting its offset.
// Empty, invalid and event-based text is not a scheduled deadline; false does not
// establish that the note has been reviewed or that its guidance is still valid.
func ReviewOverdue(reviewBy string, now time.Time) bool {
	reviewBy = strings.TrimSpace(reviewBy)
	if reviewBy == "" {
		return false
	}
	if day, err := time.Parse("2006-01-02", reviewBy); err == nil {
		return !now.Before(day.AddDate(0, 0, 1))
	}
	instant, err := time.Parse(time.RFC3339Nano, reviewBy)
	return err == nil && now.After(instant)
}
