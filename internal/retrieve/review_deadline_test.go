// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package retrieve

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func TestReviewDeadlineReachesRetrievalRanking(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name, deadline string
		want           float64
	}{
		{"past-day", now.AddDate(0, 0, -2).Format("2006-01-02"), 0.85},
		{"today", now.Format("2006-01-02"), 1},
		{"future-day", now.AddDate(0, 0, 2).Format("2006-01-02"), 1},
		{"past-instant", now.Add(-48 * time.Hour).Format(time.RFC3339), 0.85},
		{"past-offset", now.Add(-48 * time.Hour).In(time.FixedZone("offset", -7*60*60)).Format(time.RFC3339Nano), 0.85},
		{"future-instant", now.Add(48 * time.Hour).Format(time.RFC3339), 1},
		{"invalid", "0000-not-a-date", 1},
	}
	for _, tc := range tests {
		for _, reranking := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/rerank=%v", tc.name, reranking), func(t *testing.T) {
				r := buildVaultFrom(t, []noteSrc{
					{"target.md", fmt.Sprintf("---\nid: target\ntype: decision\nwhen: 2000-01-01\nreview_by: %q\n---\n# Target\nreview deadline fixture preferredmarker\n", tc.deadline)},
					{"other.md", "---\nid: other\ntype: note\nwhen: 2000-01-01\n---\n# Other\nreview deadline fixture\n"},
				})
				if reranking {
					r.EnableRerank(fakeReranker{needle: "preferredmarker"})
				}
				score := func(halfLife int) float64 {
					t.Helper()
					r.freshHalfLife = halfLife
					cards, err := r.Retrieve(context.Background(), "review deadline fixture", Options{Limit: 10})
					if err != nil {
						t.Fatal(err)
					}
					for _, c := range cards {
						if c.NoteID != "target" {
							continue
						}
						if !reranking {
							return c.Score
						}
						if !strings.Contains(c.Reason, "reranked") {
							t.Fatalf("reranker path not used: %q", c.Reason)
						}
						// With no tail the head's offset is one; compare its relevance.
						return c.Score - 1
					}
					t.Fatal("target card missing")
					return 0
				}
				off, on := score(0), score(30)
				if off <= 0 || math.Abs(on/off-tc.want) > 1e-9 {
					t.Errorf("review %q: off=%g on=%g multiplier=%g, want %g", tc.deadline, off, on, on/off, tc.want)
				}
			})
		}
	}
}
