// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"strings"
	"testing"
)

// TestRejectedLineNamesEveryReasonTheHubRefusesFor pins the one operator-facing
// sentence that two separate fixes both own.
//
// The renderer moved out of cmd/mesh and into this package (so cmd/mesh-curator could
// reach it), and at the same time the sync path guard taught the hub to refuse
// anything that is not a .md note and to REPORT the refusal in Rejected instead of
// dropping it. The move carried the pre-guard wording, which named three reasons and
// not the fourth. A refusal an operator cannot explain is a refusal they retry
// forever, so the reason list has to match what the hub actually refuses for. This
// test exists because that wording is the exact line a future merge of these two
// areas would drop again, and nothing else asserts it.
func TestRejectedLineNamesEveryReasonTheHubRefusesFor(t *testing.T) {
	sum := Summary{Head: "abcdef1234567890", Rejected: []string{".claude/settings.json"}}
	lines := sum.Lines()

	var rejLine string
	for _, l := range lines {
		if strings.Contains(l, "rejected by hub") {
			rejLine = l
		}
	}
	if rejLine == "" {
		t.Fatalf("Summary.Lines rendered no rejected line for a refused path; lines = %q", lines)
	}
	if !strings.Contains(rejLine, ".claude/settings.json") {
		t.Errorf("the rejected line does not name the path the hub refused: %q", rejLine)
	}
	for _, reason := range []string{"reason not supplied", "no write permission", "scope", "too large", "not a .md note", "binary/NUL", "path alias"} {
		if !strings.Contains(rejLine, reason) {
			t.Errorf("the rejected line never mentions %q, so an operator hit by that refusal cannot tell why "+
				"their note stayed local and will keep retrying it: %q", reason, rejLine)
		}
	}
}

func TestWatchReceiptsDeduplicatesOnlyUnchangedLocalProblems(t *testing.T) {
	blocked := BlockedNote{Path: "bad.md", Reason: "contains NUL", Hash: "first"}
	sum := Summary{Head: "head", Blocked: []BlockedNote{blocked}}
	var watch WatchReceipts
	first := strings.Join(watch.Lines(sum), "\n")
	if !strings.Contains(first, "bad.md") || !strings.Contains(first, "NOT synced") {
		t.Fatalf("first problem hidden: %s", first)
	}
	if got := watch.Lines(sum); len(got) != 0 {
		t.Fatalf("unchanged problem spam: %q", got)
	}
	watch.Lines(Summary{}) // local-only indexing between hub rounds
	if got := watch.Lines(sum); len(got) != 0 {
		t.Fatalf("local tick reset deduplication: %q", got)
	}
	if got := strings.Join(sum.Lines(), "\n"); !strings.Contains(got, "bad.md") {
		t.Fatal("one-shot receipt suppressed")
	}
	sum.Pushed = 1
	got := strings.Join(watch.Lines(sum), "\n")
	if !strings.Contains(got, "pushed 1") || !strings.Contains(got, "1 note(s) blocked") || strings.Contains(got, "bad.md") {
		t.Fatalf("movement/count hidden or detail repeated: %s", got)
	}
	sum.Pushed = 0
	sum.Blocked[0].Hash = "edited"
	if got := strings.Join(watch.Lines(sum), "\n"); !strings.Contains(got, "bad.md") {
		t.Fatal("edited problem hidden")
	}
	watch.Lines(Summary{Head: "head"}) // a completed hub round with no blocks
	if got := watch.Lines(sum); len(got) == 0 {
		t.Fatal("reappearing problem hidden")
	}
	var restarted WatchReceipts
	if got := restarted.Lines(sum); len(got) == 0 {
		t.Fatal("restart hides existing problem")
	}
	sum.Rejected = []string{"permission.md"}
	for i := 0; i < 2; i++ {
		if got := strings.Join(watch.Lines(sum), "\n"); !strings.Contains(got, "permission.md") {
			t.Fatal("unknown server refusal suppressed")
		}
	}
}
