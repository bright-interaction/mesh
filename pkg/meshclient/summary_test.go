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
	for _, reason := range []string{"no write permission", "scope", "too large", "not a .md note"} {
		if !strings.Contains(rejLine, reason) {
			t.Errorf("the rejected line never mentions %q, so an operator hit by that refusal cannot tell why "+
				"their note stayed local and will keep retrying it: %q", reason, rejLine)
		}
	}
}
