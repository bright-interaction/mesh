// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
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
			if sum.Conflicts > 0 {
				// Our push lost a race: the engine re-parked the loser. On a same-day
				// re-conflict the re-parked sibling is byte-identical to the original
				// (same path/user/content), so we just report where it now lives; on a
				// different day there may be a second sibling. Either way nothing is lost.
				fmt.Printf("synced: pushed %d, pulled %d, %d conflict(s) (HEAD %s)\n", sum.Pushed, sum.Pulled, sum.Conflicts, short8(sum.Head))
				fmt.Println("the note advanced again, so your version was re-parked (no data lost):")
				for _, s := range sum.ConflictSiblings {
					fmt.Printf("  %s  (review: mesh conflicts diff %s)\n", textdiff.Sanitize(s), s)
				}
				reindexBestEffort(vaultDir)
				return nil
			}
			if err := os.Remove(sibAbs); err != nil && !os.IsNotExist(err) {
				return err
			}
			reindexBestEffort(vaultDir)
			fmt.Printf("resolved %s: took your version and pushed it (HEAD %s).\n", textdiff.Sanitize(baseRel), short8(sum.Head))
			return nil
		},
	}
	c.Flags().BoolVar(&keepBase, "keep-base", false, "accept the current note (delete your parked sibling)")
	c.Flags().BoolVar(&takeMine, "take-mine", false, "overwrite the note with your parked version, then sync")
	return c
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

// writeFileAtomic writes b to path via a temp file + rename in the same directory.
func writeFileAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
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
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
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
