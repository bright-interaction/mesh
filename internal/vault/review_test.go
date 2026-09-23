// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package vault

import (
	"testing"
	"time"
)

func TestReviewOverdue(t *testing.T) {
	tests := []struct {
		name, deadline, now string
		want                bool
	}{
		{"day-start", "2026-09-23", "2026-09-23T00:00:00Z", false},
		{"day-end", "2026-09-23", "2026-09-23T23:59:59.999999999Z", false},
		{"next-day", "2026-09-23", "2026-09-24T00:00:00Z", true},
		{"clock-ahead-utc-day", "2026-09-23", "2026-09-24T01:00:00+02:00", false},
		{"clock-behind-utc-day", "2026-09-23", "2026-09-23T23:00:00-02:00", true},
		{"instant-before", "2026-09-23T12:00:00Z", "2026-09-23T11:59:59Z", false},
		{"instant-equal", "2026-09-23T12:00:00Z", "2026-09-23T14:00:00+02:00", false},
		{"instant-passed", "2026-09-23T12:00:00Z", "2026-09-23T12:00:00.000000001Z", true},
		{"fractional", "2026-09-23T12:00:00.000000002Z", "2026-09-23T12:00:00.000000001Z", false},
		{"positive-offset", "2026-09-23T14:00:00+02:00", "2026-09-23T12:00:01Z", true},
		{"negative-offset", "2026-09-22T23:00:00-02:00", "2026-09-23T00:30:00Z", false},
		{"whitespace", " 2026-09-22 ", "2026-09-23T00:00:00Z", true},
		{"leap-day", "2024-02-29", "2024-03-01T00:00:00Z", true},
		{"bad-day", "2025-02-29", "2026-09-23T00:00:00Z", false},
		{"bad-text", "0000-not-a-date", "2026-09-23T00:00:00Z", false},
		{"bad-instant", "2000-01-01T99:00:00Z", "2026-09-23T00:00:00Z", false},
		{"ambiguous-local-time", "2000-01-01T12:00:00", "2026-09-23T00:00:00Z", false},
		{"event", "before any release", "2026-09-23T00:00:00Z", false},
		{"empty", "", "2026-09-23T00:00:00Z", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339Nano, tc.now)
			if err != nil {
				t.Fatal(err)
			}
			if got := ReviewOverdue(tc.deadline, now); got != tc.want {
				t.Fatalf("ReviewOverdue(%q, %s) = %v, want %v", tc.deadline, now, got, tc.want)
			}
		})
	}
}
