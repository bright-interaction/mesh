// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshclient

import "fmt"

// Lines renders one sync round for an operator: a headline, then one indented line
// per thing that needs a decision. Every command that runs a round through this
// package reports it with these lines.
//
// The renderer lives HERE, next to Summary, and not inside one CLI package, because
// there is more than one binary that runs a round and tells a human about it. It used
// to live in cmd/mesh, which made it unreachable from cmd/mesh-curator, and that second
// binary duly grew its own two-field receipt that never learned about conflicts,
// refusals or a deferred remainder. One Summary, one renderer, wherever the round was
// started from.
//
// The Remaining line is the operator half of the bounded-batch rule. SyncVault caps how
// many dirty notes one round pushes and defers the rest, so a user with 12000 dirty
// notes must not read "pushed 4000" as convergence. It says what to do next, not just
// a count.
func (s Summary) Lines() []string {
	return append(s.HeadlineLines(), s.detailLines()...)
}

func (s Summary) detailLines() []string {
	var lines []string
	for _, sib := range s.ConflictSiblings {
		lines = append(lines, fmt.Sprintf("  conflict: hub version kept; your version saved at %s (resolve, then sync)", sib))
	}
	for _, sib := range s.Protected {
		lines = append(lines, fmt.Sprintf("  protected your unsaved local edit; incoming hub version saved at %s", sib))
	}
	for _, rej := range s.Rejected {
		// The reason list includes "not a .md note" because the hub-side sync path guard
		// refuses anything outside the note tree and reports it in Rejected rather than
		// dropping it silently. A refusal an operator cannot explain is a refusal they
		// will retry forever, so the wording names every reason the hub actually uses.
		lines = append(lines, fmt.Sprintf("  rejected by hub (reason not supplied; possible no write permission, scope, too large, binary/NUL content, not a .md note, or path alias): %s -- kept local, will retry", rej))
	}
	for _, note := range s.Blocked {
		lines = append(lines, fmt.Sprintf("  blocked locally: %s -- %s; kept local and NOT synced; correct the file to resume uploads", note.Path, note.Reason))
	}
	return lines
}

// HeadlineLines renders the part of a round that is true wherever the round was
// triggered from: what moved, and whether the push was complete.
//
// It is split out because `mesh conflicts resolve --take-mine` owns its own receipt
// wording for the note it just rescued, but still has to tell the truth about the
// round that carried it. One format string, every caller.
func (s Summary) HeadlineLines() []string {
	lines := []string{fmt.Sprintf("synced: pushed %d, pulled %d, %d conflict(s) (HEAD %s)",
		s.Pushed, s.Pulled, s.Conflicts, shortSHA(s.Head))}
	if s.Remaining > 0 {
		lines = append(lines, fmt.Sprintf("  %d more changed note(s) are still queued and NOT on the hub yet "+
			"(one round pushes a bounded batch); run `mesh sync` again to send them", s.Remaining))
	}
	if len(s.Blocked) > 0 {
		lines = append(lines, fmt.Sprintf("  %d note(s) blocked by local content checks; NOT uploaded", len(s.Blocked)))
	}
	return lines
}

// Moved reports whether a round is worth a log line under a watch loop, which idles
// at a periodic reconcile and would otherwise print a no-op every tick. Remaining
// counts: a round that pushed a full batch and deferred the rest has moved, and the
// deferred tail is exactly what the operator needs to be told about.
func (s Summary) Moved() bool {
	return s.Pushed > 0 || s.Pulled > 0 || s.Conflicts > 0 ||
		s.Remaining > 0 || len(s.Protected) > 0 || len(s.Rejected) > 0 || len(s.Blocked) > 0
}

// WatchReceipts reports unchanged local content problems once, rather than on
// every safety tick. It does not alter sync state or retries. One-shot callers
// always use Summary.Lines and see every problem. A restart, changed bytes, or a
// problem that disappears and returns produces a fresh diagnostic.
type WatchReceipts struct {
	blocked map[string]BlockedNote
}

func (r *WatchReceipts) Lines(sum Summary) []string {
	// Local-only index ticks return a zero Summary. They provide no evidence
	// that a blocked upload was fixed and must not clear the last hub receipt.
	if sum.Head == "" && !sum.Moved() {
		return nil
	}
	current := make(map[string]BlockedNote, len(sum.Blocked))
	var fresh []BlockedNote
	for _, note := range sum.Blocked {
		current[note.Path] = note
		if previous, ok := r.blocked[note.Path]; !ok || previous != note {
			fresh = append(fresh, note)
		}
	}
	r.blocked = current
	headlines := sum.HeadlineLines() // keep the total outstanding count truthful
	sum.Blocked = fresh
	if !sum.Moved() {
		return nil
	}
	return append(headlines, sum.detailLines()...)
}

// shortSHA abbreviates a commit sha for a receipt, and names the empty case rather
// than printing nothing where a sha belongs.
func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	if sha == "" {
		return "(none)"
	}
	return sha
}
