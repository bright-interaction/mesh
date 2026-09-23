// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"fmt"
	"testing"
	"time"
)

func TestHealthReviewDeadlineFormats(t *testing.T) {
	now := time.Date(2026, 9, 23, 0, 30, 0, 0, time.UTC)
	tests := []struct {
		id, deadline string
		overdue      bool
	}{
		{"past-day", "2026-09-22", true},
		{"today", "2026-09-23", false},
		{"future-day", "2026-09-24", false},
		{"past-instant", "2026-09-23T00:29:59Z", true},
		{"same-instant", "2026-09-23T00:30:00Z", false},
		{"future-instant", "2026-09-23T00:30:01Z", false},
		{"past-positive-offset", "2026-09-23T02:00:00+02:00", true},
		{"future-negative-offset", "2026-09-22T23:00:00-02:00", false},
		{"invalid-date", "2000-02-30", false},
		{"invalid-low-text", "0000-not-a-date", false},
		{"event-based", "before release", false},
		{"none", "", false},
	}
	dir := t.TempDir()
	var notes []*ParsedNote
	for _, tc := range tests {
		notes = append(notes, writeNote(t, dir, "notes/"+tc.id+".md", fmt.Sprintf(
			"---\nid: %s\ntype: note\nwhen: 2026-09-22\nreview_by: %q\n---\n# Review deadline\n", tc.id, tc.deadline)))
	}
	g, _ := BuildGraph(notes)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.IndexVault(notes, g); err != nil {
		t.Fatal(err)
	}
	findings, err := s.ComputeHealth(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, finding := range findings {
		if finding.Issue == "overdue" {
			got[finding.NoteID] = true
		}
	}
	for _, tc := range tests {
		if got[tc.id] != tc.overdue {
			t.Errorf("%s deadline %q: overdue = %v, want %v", tc.id, tc.deadline, got[tc.id], tc.overdue)
		}
	}
}
