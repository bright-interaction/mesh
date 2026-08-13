// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/merge"
	"github.com/bright-interaction/mesh/internal/textdiff"
	"github.com/bright-interaction/mesh/internal/vault"
	"github.com/bright-interaction/mesh/pkg/meshclient"
	"github.com/spf13/cobra"
)

// conflictsCmd is the LOCAL sync-conflict janitor (S2.2): on the machine that LOST
// a true conflict, list / diff / resolve the parked *.sync-conflict siblings.
// Fully offline: it only reads local files and (on take-mine) calls the same
// SyncVault the sync command uses. Naming is base/mine: "base" is the note now at
// the path (the live team/curator version), "mine" is the parked loser.
func conflictsCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "conflicts [vault]",
		Short: "List, diff, and resolve local sync-conflict siblings",
		Long:  "When a true conflict happens, the hub version is kept at the note's path and your overwrite is parked in a *.sync-conflict sibling. These commands let you review your parked version against the current note (which may be a curator merge) and either keep the current note or take yours.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { // bare `mesh conflicts` lists
			return listConflicts(vaultArg(args), asJSON)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit rows as JSON")
	c.AddCommand(conflictsListCmd(), conflictsDiffCmd(), conflictsResolveCmd())
	return c
}

type conflictRow struct {
	Sibling string `json:"sibling"`
	Base    string `json:"base"`
	User    string `json:"user"`
	Date    string `json:"date"`
	Status  string `json:"status"`
}

func conflictsListCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "list [vault]",
		Short: "List parked sync-conflict siblings and their status",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return listConflicts(vaultArg(args), asJSON)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit rows as JSON")
	return c
}

func listConflicts(vaultDir string, asJSON bool) error {
	sibs, err := vault.WalkConflictSiblings(vaultDir)
	if err != nil {
		return err
	}
	rows := make([]conflictRow, 0, len(sibs))
	for _, sib := range sibs {
		rel, _ := filepath.Rel(vaultDir, sib)
		baseRel, ok := merge.BasePath(rel)
		user, date := siblingMeta(sib)
		row := conflictRow{Sibling: rel, Base: baseRel, User: user, Date: date}
		switch {
		case !ok:
			row.Base = ""
			row.Status = "orphan (unparseable name; no base to restore to, delete or rename by hand)"
		default:
			baseAbs := filepath.Join(vaultDir, filepath.FromSlash(baseRel))
			baseBytes, berr := os.ReadFile(baseAbs)
			sibBytes, _ := os.ReadFile(sib)
			switch {
			case os.IsNotExist(berr):
				row.Status = "base missing (take-mine restores it)"
			case berr != nil:
				row.Status = "base unreadable: " + berr.Error()
			case bytes.Equal(baseBytes, sibBytes):
				row.Status = "identical to base (safe to drop: resolve --keep-base)"
			default:
				row.Status = "base present (diff, then resolve)"
			}
		}
		rows = append(rows, row)
	}
	// Group by base path for readability (multi-loser aware).
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Base != rows[j].Base {
			return rows[i].Base < rows[j].Base
		}
		return rows[i].Sibling < rows[j].Sibling
	})
	if asJSON {
		b, _ := json.MarshalIndent(rows, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	if len(rows) == 0 {
		fmt.Println("no local conflicts. (curator activity across the team: mesh curator log)")
		return nil
	}
	fmt.Printf("%d local conflict(s):\n", len(rows))
	for i, r := range rows {
		base := r.Base
		if base == "" {
			base = "(unknown base)"
		}
		fmt.Printf("  %d. %s\n", i+1, textdiff.Sanitize(base))
		fmt.Printf("     mine: %s (%s, %s)\n", textdiff.Sanitize(r.Sibling), orNA(textdiff.Sanitize(r.User)), orNA(r.Date))
		fmt.Printf("     %s\n", r.Status)
	}
	fmt.Println("next: mesh conflicts diff <sibling>  then  mesh conflicts resolve <sibling> --keep-base|--take-mine")
	return nil
}

func conflictsDiffCmd() *cobra.Command {
	var ctxLines int
	var full bool
	c := &cobra.Command{
		Use:   "diff <sibling> [vault]",
		Short: "Show your parked version (mine) against the current note (base)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			vaultDir := "."
			if len(args) == 2 {
				vaultDir = args[1]
			}
			sibAbs, baseAbs, baseRel, err := validateSibling(vaultDir, args[0])
			if err != nil {
				return err
			}
			mine, err := os.ReadFile(sibAbs)
			if err != nil {
				return err
			}
			base, baseErr := os.ReadFile(baseAbs)
			if baseErr != nil && !os.IsNotExist(baseErr) {
				return baseErr
			}
			if !merge.IsText(mine) || (len(base) > 0 && !merge.IsText(base)) {
				return fmt.Errorf("refusing to diff: one side is binary or oversize")
			}
			opts := textdiff.Options{Context: ctxLines, Full: full, Color: colorEnabled()}
			fmt.Printf("--- base (current team/curator version: %s)\n", textdiff.Sanitize(baseRel))
			user, date := siblingMeta(sibAbs)
			fmt.Printf("+++ mine (your parked version, %s %s)\n", orNA(textdiff.Sanitize(user)), orNA(date))
			if os.IsNotExist(baseErr) {
				fmt.Println("note: the base note was deleted on the hub; resolve --take-mine would resurrect it.")
			}
			out := textdiff.Unified(base, mine, opts)
			if out == "" {
				fmt.Println("(no differences: your version matches the current note; resolve --keep-base to drop the sibling)")
				return nil
			}
			fmt.Print(out)
			return nil
		},
	}
	c.Flags().IntVar(&ctxLines, "context", 3, "lines of context around a change")
	c.Flags().BoolVar(&full, "full", false, "show the whole note, not just changed regions")
	return c
}

func conflictsResolveCmd() *cobra.Command {
	var keepBase, takeMine bool
	c := &cobra.Command{
		Use:   "resolve <sibling> --keep-base|--take-mine [vault]",
		Short: "Resolve a conflict: keep the current note, or take your parked version",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if keepBase == takeMine { // both or neither
				return fmt.Errorf("choose exactly one of --keep-base or --take-mine")
			}
			vaultDir := "."
			if len(args) == 2 {
				vaultDir = args[1]
			}
			sibAbs, baseAbs, baseRel, err := validateSibling(vaultDir, args[0])
			if err != nil {
				return err
			}
			_, baseErr := os.Stat(baseAbs)
			baseExists := baseErr == nil

			if keepBase {
				if !baseExists {
					return fmt.Errorf("base note %s does not exist: keeping it would lose your only copy. Use --take-mine to restore it", baseRel)
				}
				if fi, serr := os.Stat(sibAbs); serr == nil && !fi.Mode().IsRegular() {
					return fmt.Errorf("%s is not a regular file", args[0])
				}
				if err := os.Remove(sibAbs); err != nil {
					if os.IsNotExist(err) {
						fmt.Println("already resolved (sibling gone).")
						return nil
					}
					return err
				}
				fmt.Printf("resolved %s: kept the current note, dropped your parked version.\n", baseRel)
				return nil
			}

			// take-mine: overwrite base from the sibling, sync, then drop the sibling
			// only if the push lands. Never delete before sync confirms (no loss).
			mine, err := os.ReadFile(sibAbs)
			if err != nil {
				return err
			}
			fmt.Printf("taking your version for %s. note: this overwrites the current note (which may be a curator/teammate merge); if it advanced, sync will re-park your version safely.\n", textdiff.Sanitize(baseRel))
			if err := writeFileAtomic(baseAbs, mine); err != nil {
				return err
			}
			// The push is the commit point: separate it from the (best-effort) reindex
			// so a reindex failure is never mistaken for a failed push (which would
			// wrongly keep the sibling and tell the user to retry an already-landed push).
			sum, serr := meshclient.SyncVault(vaultDir)
			if serr != nil {
				fmt.Printf("base written, but the push failed: %v\n", serr)
				fmt.Printf("your parked version is KEPT at %s; re-run resolve --take-mine when the hub is reachable.\n", args[0])
				return serr
			}
			// Both branches below ask about the SAME round, and both must ask about the
			// note the operator is rescuing, never about the vault:
			//
			//	takeMineRejected      = the hub refused OUR path
			//	takeMineReConflicted  = the round re-parked OUR path
			//
			// ORDER MATTERS because a round can answer yes to both (our note refused by
			// an ACL while it also loses a race), so whichever branch is tested first
			// decides the receipt and the exit code. Conflicts-first re-opened the defect
			// the rejection check was added to close: control took the conflicts branch,
			// printed "your version was re-parked" naming a sibling belonging to a
			// DIFFERENT note, and exited 0 on a push that never landed. So the refusal is
			// asked first, and the round's other conflicts are reported inside whichever
			// receipt owns the round instead of replacing it.
			//
			// The second branch used to read `sum.Conflicts > 0`, which is a counter for
			// the whole round rather than a fact about this note, and that was the same
			// defect wearing the other hat: a landed push reported as re-parked. See
			// takeMineReConflicted.
			//
			// The hub REFUSES a path outright for viewer role, folder ACL, note scope,
			// or oversize/binary content. A refusal is not a conflict, so sum.Conflicts
			// is 0 in the plain case and control used to fall straight through to the
			// delete below: the parked sibling, which is the operator's only evidence of
			// the divergence, was destroyed and the receipt said the team could see the
			// rescued version. The bytes themselves survive at the note's own path and
			// SyncVault keeps them dirty for the next round, so this is a false receipt
			// rather than byte loss, but it breaks the invariant above: drop the sibling
			// only if the push LANDS.
			if takeMineRejected(baseRel, sum) {
				for _, line := range takeMineRejectedReceipt(baseRel, args[0], sum) {
					fmt.Println(line)
				}
				reindexBestEffort(vaultDir)
				return fmt.Errorf("the hub refused %s, so nothing was pushed and your parked version was kept", textdiff.Sanitize(baseRel))
			}
			if takeMineReConflicted(baseRel, sum) {
				for _, line := range takeMineReConflictedReceipt(baseRel, sum) {
					fmt.Println(line)
				}
				reindexBestEffort(vaultDir)
				return nil
			}
			if err := os.Remove(sibAbs); err != nil && !os.IsNotExist(err) {
				return err
			}
			reindexBestEffort(vaultDir)
			for _, line := range takeMineReceipt(baseRel, sum) {
				fmt.Println(line)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&keepBase, "keep-base", false, "accept the current note (delete your parked sibling)")
	c.Flags().BoolVar(&takeMine, "take-mine", false, "overwrite the note with your parked version, then sync")
	return c
}

// takeMineReceipt renders what `mesh conflicts resolve --take-mine` tells the user
// after the push returned without a re-conflict.
//
// "pushed it" is only true when the round was complete. SyncVault pushes a bounded
// batch and defers the rest, and the resolved note is an ordinary dirty path in that
// outbox, so on a vault with a backlog it can be in the deferred tail: the sibling is
// dropped, the receipt says the hub has the rescued version, and the hub does not.
// Nothing is lost (the bytes are at the note's own path and stay dirty until a later
// round sends them) but the receipt has to say so, or the user stops here believing
// the team can see it.
//
// It is only reached when OUR note neither conflicted nor was refused, so every conflict
// the round reports belongs to someone else's note and is named as such. Saying nothing
// about them would leave a sibling on disk that no receipt explains, right after telling
// the operator this note is settled.
func takeMineReceipt(baseRel string, sum meshclient.Summary) []string {
	lines := []string{fmt.Sprintf("resolved %s: took your version and pushed it (HEAD %s).",
		textdiff.Sanitize(baseRel), short8(sum.Head))}
	if sum.Remaining > 0 {
		lines = append(lines, fmt.Sprintf("  the round was bounded: %d changed note(s) are still queued and "+
			"NOT on the hub yet, and this note may be one of them; run `mesh sync` again.", sum.Remaining))
	}
	return append(lines, otherConflictLines(sum.Conflicts, sum.ConflictSiblings)...)
}

// takeMineReConflicted reports whether the round re-parked THE NOTE the operator is
// rescuing, as opposed to some other note that lost its own race in the same round.
//
// sum.Conflicts is a counter for the whole ROUND, so reading it bare answers a question
// about the vault with a sentence about one note. A round where our push LANDED while an
// unrelated note conflicted took this branch: it printed "your version was re-parked" over
// a push that had in fact reached the hub, handed the operator a sibling path belonging to
// that other note, and skipped the delete, so the resolved note's own stale sibling stayed
// on disk and `mesh conflicts list` kept reporting it as unresolved. The offered path is
// the dangerous part: `resolve --keep-base` on it drops the OTHER note's parked version,
// which is that note's only copy of its divergence.
//
// Attribution is per sibling, the same way internal/curator/curator.go:pushOutcome does it
// for the same Summary, and it fails SAFE in both unclear cases, because keeping a sibling
// costs the operator one command while dropping one wrongly costs a version: a Conflicts
// count larger than the sibling list that explains it, and a sibling whose base path
// cannot be recovered, are both treated as ours.
func takeMineReConflicted(baseRel string, sum meshclient.Summary) bool {
	_, _, ours := roundConflicts(baseRel, sum)
	return ours
}

// roundConflicts splits the round's re-parked siblings into the ones that belong to the
// resolved note and the ones that belong to other notes, and reports whether the resolved
// note is among them. It is the ONE place the attribution is decided, so the predicate
// above and the receipt below cannot drift into two different answers.
//
// Both unclear shapes resolve toward "ours": a Conflicts count larger than the sibling
// list that explains it (the list cannot say which one we are), and a sibling whose base
// path merge.BasePath cannot recover. Over-reporting costs the operator a second look;
// under-reporting invites them to drop another note's only parked copy with --keep-base.
func roundConflicts(baseRel string, sum meshclient.Summary) (mine, others []string, ours bool) {
	if sum.Conflicts == 0 {
		return nil, nil, false
	}
	if sum.Conflicts > len(sum.ConflictSiblings) {
		return sum.ConflictSiblings, nil, true
	}
	want := path.Clean(filepath.ToSlash(baseRel))
	for _, sib := range sum.ConflictSiblings {
		base, ok := merge.BasePath(path.Clean(filepath.ToSlash(sib)))
		if !ok || path.Clean(base) == want {
			mine = append(mine, sib)
			continue
		}
		others = append(others, sib)
	}
	return mine, others, len(mine) > 0
}

// takeMineReConflictedReceipt renders the branch where OUR push lost a race and the
// engine re-parked our version again.
//
// On a same-day re-conflict the re-parked sibling is byte-identical to the original
// (same path, user and content), so this reports where our version now lives; on a
// different day there may be a second sibling. Either way nothing is lost.
//
// The headline comes from the SAME renderer `mesh sync` and its watch loop use. This
// site used to hand-roll it, which is why it never learned to mention a deferred
// remainder: a bounded round here reported a complete push. It also used to print every
// sibling in the round under "your version", including notes the operator had never
// touched, so `resolve --keep-base` on one of those offered paths dropped that note's
// only parked copy. Ours and theirs are named separately now.
func takeMineReConflictedReceipt(baseRel string, sum meshclient.Summary) []string {
	mine, others, _ := roundConflicts(baseRel, sum)
	lines := syncHeadlineLines(sum)
	lines = append(lines, "the note advanced again, so your version was re-parked (no data lost):")
	for _, s := range mine {
		lines = append(lines, fmt.Sprintf("  %s  (review: mesh conflicts diff %s)", textdiff.Sanitize(s), s))
	}
	return append(lines, otherConflictLines(len(others), others)...)
}

// otherConflictLines names the notes OTHER than the resolved one that the same round
// re-parked. Both receipts that can own a round without our note conflicting need it (the
// hub refused us, or our push landed): the round still created a sibling on disk, and a
// *.sync-conflict file that no receipt ever mentioned reads as this note's unfinished
// business. Callers pass the round's count and siblings, which are all other notes' by
// construction at both call sites.
func otherConflictLines(n int, sibs []string) []string {
	if n <= 0 {
		return nil
	}
	lines := []string{fmt.Sprintf("  separately, %d OTHER note(s) conflicted in the same round and were re-parked "+
		"(this is not your note and does not change the outcome above); review them with `mesh conflicts list`:", n)}
	for _, s := range sibs {
		lines = append(lines, fmt.Sprintf("    %s", textdiff.Sanitize(s)))
	}
	return lines
}

// takeMineRejected reports whether the round the resolve just ran REFUSED the note
// the operator was rescuing. Summary.Rejected carries the paths the hub would not
// accept (viewer role, folder ACL, note scope, oversize or non-text content); the
// note's bytes stay on disk and stay dirty, but they are not on the hub and will not
// be until the refusal is lifted.
//
// Only THIS path counts. A rejection somewhere else in the same round says nothing
// about the resolved note, and treating it as one would strand a sibling forever on
// any vault that holds one permanently unwritable folder.
//
// Both sides are cleaned to slash form before comparing: baseRel is reconstructed
// locally by validateSibling while the entries come back from the hub, so the same
// note can arrive spelled slightly differently on Windows or after a "./" prefix.
func takeMineRejected(baseRel string, sum meshclient.Summary) bool {
	want := path.Clean(filepath.ToSlash(baseRel))
	for _, rel := range sum.Rejected {
		if path.Clean(filepath.ToSlash(rel)) == want {
			return true
		}
	}
	return false
}

// takeMineRejectedReceipt renders what the operator is told when the hub refused the
// resolved path. It has to carry three facts, because the operator stops reading here:
// the push did NOT land, the parked sibling was kept (so the evidence of the divergence
// is still there), and the local bytes stay queued so a later sync retries them.
//
// A refusal and a conflict are independent outcomes of the same round, so a round that
// refused OUR path can also have re-parked some OTHER note. That is reported here as a
// trailing line rather than in its own branch: the refusal is what the operator asked
// about and it owns the receipt, but the round still changed the vault and saying nothing
// would leave a fresh sibling on disk that no receipt ever mentioned.
func takeMineRejectedReceipt(baseRel, sibling string, sum meshclient.Summary) []string {
	lines := []string{
		fmt.Sprintf("the hub REFUSED %s: your version was NOT pushed and the team cannot see it (HEAD %s).",
			textdiff.Sanitize(baseRel), short8(sum.Head)),
		"  a refusal means write access, not a merge: a viewer role, a folder ACL, the note's scope, " +
			"a note that is too large or not text, or a path the hub does not accept (it takes .md " +
			"notes under the vault, nothing hidden or reserved). Ask an admin for write access on this path.",
		fmt.Sprintf("  your version is KEPT in both places: at %s locally and in the parked sibling %s. Nothing was deleted.",
			textdiff.Sanitize(baseRel), textdiff.Sanitize(sibling)),
		"  the note stays queued as a local change, so every later `mesh sync` retries the push; " +
			"re-run resolve --take-mine once the refusal is lifted to drop the sibling.",
	}
	return append(lines, otherConflictLines(sum.Conflicts, sum.ConflictSiblings)...)
}

// validateSibling enforces that arg is a real conflict sibling inside vaultDir and
// returns the absolute sibling path, the absolute base path, and the vault-relative
// base path. It is the single guard before any read/delete on a user-supplied path.
func validateSibling(vaultDir, arg string) (sibAbs, baseAbs, baseRel string, err error) {
	vaultAbs, err := filepath.Abs(vaultDir)
	if err != nil {
		return "", "", "", err
	}
	sibAbs, err = filepath.Abs(arg)
	if err != nil {
		return "", "", "", err
	}
	rel, err := filepath.Rel(vaultAbs, sibAbs)
	if err != nil || !filepath.IsLocal(rel) {
		return "", "", "", fmt.Errorf("%s is outside the vault %s", arg, vaultDir)
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == ".git" || part == ".mesh" {
			return "", "", "", fmt.Errorf("%s is in a reserved directory", arg)
		}
	}
	if !vault.IsConflictSibling(filepath.Base(sibAbs)) {
		return "", "", "", fmt.Errorf("%s is not a sync-conflict sibling", arg)
	}
	// Note: existence is NOT checked here (so keep-base can report "already
	// resolved" idempotently); each consumer Stats/reads as it needs.
	baseRel, ok := merge.BasePath(filepath.ToSlash(rel))
	if !ok {
		return "", "", "", fmt.Errorf("cannot reconstruct the base note path from %s", arg)
	}
	baseAbs = filepath.Join(vaultAbs, filepath.FromSlash(baseRel))
	return sibAbs, baseAbs, baseRel, nil
}

// reindexBestEffort reindexes the vault after a successful push so search reflects
// the result. It is best-effort: the push already landed, so a reindex failure
// (e.g. the db is locked by a concurrent `mesh sync --watch`) is a warning, not a
// failure of the resolve. The next sync/index reconciles it.
func reindexBestEffort(vaultDir string) {
	store, err := index.Open(vaultDir)
	if err == nil {
		_, err = index.Reconcile(store, vaultDir)
		store.Close()
	}
	if err != nil {
		fmt.Printf("note: reindex failed (%v); run `mesh index` to refresh search.\n", err)
	}
}

// writeFileAtomic writes b to path via a temp file + rename in the same directory,
// and fsyncs both the data and the parent directory so the resolved note survives a
// power cut.
//
// Temp+rename alone buys crash-atomicity for a concurrent READER, not durability.
// This is the note-byte writer for `mesh conflicts resolve`, which is the ONE place
// where the losing side of a true conflict is restored: take-mine writes the parked
// sibling's bytes back over the note and then deletes the sibling. If the rename
// reaches the disk and the data blocks do not, the sibling is already gone and the
// note reverts to the hub version, so the user's rescued edit is lost with nothing
// left on disk to rescue it from a second time.
//
// This is the third of FOUR copies of this write; see the note on
// pkg/meshclient/vault.go writeFileAtomic for the full list, and
// TestEveryTempRenameWriterFsyncs in atomic_write_durability_test.go for the guard
// that finds any new one.
//
// The destination's current permission bits are preserved when it already exists, and a
// new note lands 0644, the mode every other note writer in the estate uses. This one was
// never an os.WriteFile, so 0644 for a new file is a chosen default and not a restoration:
// it exists so a resolved note is not the odd 0600 file in a 0644 vault. A rename installs
// the TEMP file's mode over the destination and os.CreateTemp makes its file 0600. This
// writer lands on a note the user has been editing by hand, which is the file most likely
// to carry permissions somebody chose on purpose, so narrowing it here is the least
// acceptable place in the tree to do it.
func writeFileAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	perm := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		perm = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, ".mesh-resolve-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	// CreateTemp makes the file 0600; match the mode the note is meant to land with,
	// BEFORE the rename publishes it.
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	// Flush the data to the device BEFORE the rename publishes the name.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	syncDir(dir)
	return nil
}

// syncDir fsyncs a directory so a rename that just landed in it survives a power cut.
// Best effort on purpose: opening or fsyncing a directory handle is a no-op or an
// error on some platforms and network filesystems (Windows cannot open one at all),
// and that is not a failed write, so the bytes still count as written and the refusal
// is only logged. The data itself is already fsynced by the caller.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		slog.Debug("conflicts: cannot open the note directory to fsync it", "dir", dir, "err", err)
		return
	}
	if err := d.Sync(); err != nil {
		slog.Debug("conflicts: directory fsync not supported here", "dir", dir, "err", err)
	}
	d.Close()
}

// siblingMeta best-effort parses the user and date out of a sibling filename for
// display only (parsing failures just show "?"). Format:
// <stem>.sync-conflict-YYYYMMDD-user-16hex<ext>.
func siblingMeta(sibling string) (user, date string) {
	bn := filepath.Base(sibling)
	stem := strings.TrimSuffix(bn, filepath.Ext(bn))
	idx := strings.LastIndex(stem, ".sync-conflict-")
	if idx < 0 {
		return "", ""
	}
	rest := stem[idx+len(".sync-conflict-"):] // YYYYMMDD-user-16hex
	parts := strings.Split(rest, "-")
	if len(parts) < 3 {
		return "", ""
	}
	date = parts[0]
	user = strings.Join(parts[1:len(parts)-1], "-")
	if len(date) == 8 {
		date = date[:4] + "-" + date[4:6] + "-" + date[6:]
	}
	return user, date
}

func colorEnabled() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func orNA(s string) string {
	if strings.TrimSpace(s) == "" {
		return "?"
	}
	return s
}
