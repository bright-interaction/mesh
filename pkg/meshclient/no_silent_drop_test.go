// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A `continue` inside a loop over a user's notes is how work disappears.
//
// This class has now cost twice. A 2026-06-22 fix taught the oversize/binary branch of
// the hub push loop to report a skipped item; the sibling branch 25 lines above it kept
// its bare `continue`, so an item the hub could not path-validate was dropped AND the
// client baselined it as synced, while the summary said "Pushed: 1". The same shape sits
// in the client's conflict-preservation loop.
//
// The rule: in these loops, every `continue` must either report the path (append to a
// rejected/summary list) or be explicitly marked safe with a `//safe-skip:` note saying
// why no bytes are at risk. This test fails on a bare one, so the next sibling site
// cannot be born silent.
func TestNoSilentDropInByteCarryingLoops(t *testing.T) {
	// file -> the functions whose loops carry a user's bytes
	guarded := map[string][]string{
		"vault.go": {"applyDeltas", "writeConflictSiblings", "dropFullReconcileOrphans", "dropTombstoned"},
	}
	fnStart := regexp.MustCompile(`^func\s+(?:\([^)]*\)\s*)?(\w+)`)

	for file, fns := range guarded {
		want := map[string]bool{}
		for _, f := range fns {
			want[f] = true
		}
		raw, err := os.ReadFile(filepath.Join(".", file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		lines := strings.Split(string(raw), "\n")
		cur := ""
		for i, l := range lines {
			if m := fnStart.FindStringSubmatch(l); m != nil {
				cur = m[1]
			}
			if !want[cur] || strings.TrimSpace(l) != "continue" {
				continue
			}
			// Look back for a reason: a report, or an explicit safe-skip marker.
			lo := i - 6
			if lo < 0 {
				lo = 0
			}
			ctx := strings.Join(lines[lo:i+1], "\n")
			reported := strings.Contains(ctx, "rejected = append") ||
				strings.Contains(ctx, "parked = append") ||
				strings.Contains(ctx, "dropped = append") ||
				strings.Contains(ctx, "//safe-skip:")
			if !reported {
				t.Errorf("%s:%d in %s(): a bare `continue` in a loop over the user's notes. "+
					"Either report the path (append to a rejected/summary list) or add a "+
					"`//safe-skip: <why no bytes are at risk>` comment. This is the shape "+
					"that silently dropped a note and recorded it as synced.",
					file, i+1, cur)
			}
		}
	}
}
