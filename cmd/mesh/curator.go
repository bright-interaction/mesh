// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/merge"
	"github.com/bright-interaction/mesh/internal/textdiff"
	"github.com/bright-interaction/mesh/pkg/meshclient"
	"github.com/spf13/cobra"
)

// curatorCmd is the HUB-anchored, team-wide audit of the BYOAI sync-curator (S2.2):
// log / show / accept. Unlike `mesh conflicts` (which only sees the local loser's
// siblings), this works on ANY client, because the curator's merges and failures
// are recorded on the hub. It is how the N-1 teammates who never produced a sibling
// (and anyone reviewing a failed job) can see what the curator did.
func curatorCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "curator",
		Short: "Review what the BYOAI sync-curator merged (and failed on) across the team",
		Long:  "The curator merges true conflicts on the hub and commits the result back, so the merged note arrives in everyone's vault via normal sync. These commands show that activity (log), the actual merge for one job (show), and acknowledge a merge already applied (accept). The hub stays AI-free; this only reads what it already recorded.",
	}
	c.AddCommand(curatorLogCmd(), curatorShowCmd(), curatorAcceptCmd())
	return c
}

func curatorLogCmd() *cobra.Command {
	var limit int
	var statusFilter string
	var asJSON bool
	c := &cobra.Command{
		Use:   "log [vault]",
		Short: "List recent curator activity (resolved merges and failed jobs)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := meshclient.ClientForVault(vaultArg(args))
			if err != nil {
				return err
			}
			jobs, err := cl.CurationActivity(limit)
			if err != nil {
				return err
			}
			if statusFilter != "" {
				want := map[string]bool{}
				for _, s := range strings.Split(statusFilter, ",") {
					want[strings.TrimSpace(s)] = true
				}
				filtered := jobs[:0]
				for _, j := range jobs {
					if want[j.Status] {
						filtered = append(filtered, j)
					}
				}
				jobs = filtered
			}
			if asJSON {
				b, _ := json.MarshalIndent(jobs, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			if len(jobs) == 0 {
				fmt.Println("no curator activity yet.")
				return nil
			}
			fmt.Printf("%d curator job(s), newest first:\n", len(jobs))
			for _, j := range jobs {
				when := whenStr(j.ResolvedAt)
				if j.ResolvedAt == 0 {
					when = whenStr(j.CreatedAt)
				}
				if j.Status == "failed" {
					fmt.Printf("  [%d] FAILED  %s  (conflict by %s, %s)\n", j.ID, textdiff.Sanitize(j.Path), orNA(textdiff.Sanitize(j.User)), when)
					if j.LastError != "" {
						fmt.Printf("        reason: %s\n", textdiff.Sanitize(j.LastError))
					}
					fmt.Printf("        needs manual merge: mesh curator show %d\n", j.ID)
					continue
				}
				fmt.Printf("  [%d] merged  %s  (conflict by %s, %s)\n", j.ID, textdiff.Sanitize(j.Path), orNA(textdiff.Sanitize(j.User)), when)
				fmt.Printf("        %s -> %s   review: mesh curator show %d\n", short8(j.BaseSHA), short8(j.ResolvedHead), j.ID)
			}
			return nil
		},
	}
	c.Flags().IntVar(&limit, "limit", 50, "max jobs to list (hub caps at 200)")
	c.Flags().StringVar(&statusFilter, "status", "", "comma-separated filter: resolved,failed")
	c.Flags().BoolVar(&asJSON, "json", false, "emit jobs as JSON")
	return c
}

func curatorShowCmd() *cobra.Command {
	var dumpLoser bool
	c := &cobra.Command{
		Use:   "show <id> [vault]",
		Short: "Show what the curator did for one job (merge diff, or recover a failed loser)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseJobID(args[0])
			if err != nil {
				return err
			}
			vaultDir := "."
			if len(args) == 2 {
				vaultDir = args[1]
			}
			cl, err := meshclient.ClientForVault(vaultDir)
			if err != nil {
				return err
			}
			job, err := cl.CurationJob(id)
			if err != nil {
				return err
			}
			loser, derr := base64.StdEncoding.DecodeString(job.IncomingB64)
			if derr != nil {
				return fmt.Errorf("decode loser bytes: %w", derr)
			}
			if dumpLoser {
				_, werr := os.Stdout.Write(loser)
				return werr
			}

			if job.Status == "failed" {
				fmt.Printf("job %d FAILED on %s (conflict by %s).\n", job.ID, textdiff.Sanitize(job.Path), orNA(textdiff.Sanitize(job.User)))
				if job.LastError != "" {
					fmt.Printf("reason: %s\n", textdiff.Sanitize(job.LastError))
				}
				fmt.Println("no merge happened: the note at this path is the raw winning version, not a merge.")
				fmt.Printf("recover the losing version for a hand-merge: mesh curator show %d --loser > recovered.md\n", job.ID)
				return nil
			}

			// Resolved: diff the loser against the live note (the merged result that
			// synced to every client). The live note may have advanced past the
			// recorded resolved_head if teammates edited it since. job.Path is
			// hub-supplied, so guard it against traversal before reading off disk.
			if !safeRelPath(job.Path) {
				return fmt.Errorf("the hub returned an unsafe note path %q; refusing to read it", job.Path)
			}
			liveAbs := filepath.Join(vaultDir, filepath.FromSlash(job.Path))
			live, lerr := os.ReadFile(liveAbs)
			if lerr != nil {
				return fmt.Errorf("read live note %s (run `mesh sync` first): %w", textdiff.Sanitize(job.Path), lerr)
			}
			fmt.Printf("job %d: curator merged %s (conflict by %s)\n", job.ID, textdiff.Sanitize(job.Path), orNA(textdiff.Sanitize(job.User)))
			fmt.Printf("resolved at HEAD %s\n", short8(job.ResolvedHead))
			fmt.Printf("--- mine (the losing version)\n")
			fmt.Printf("+++ merged (the current note; may have advanced past %s if edited since)\n", short8(job.ResolvedHead))
			if !merge.IsText(loser) || !merge.IsText(live) {
				fmt.Println("(cannot diff: one side is binary or oversize)")
				return nil
			}
			out := textdiff.Unified(loser, live, textdiff.Options{Color: colorEnabled()})
			if out == "" {
				fmt.Println("(the current note is identical to the losing version)")
				return nil
			}
			fmt.Print(out)
			return nil
		},
	}
	c.Flags().BoolVar(&dumpLoser, "loser", false, "write the losing version's bytes to stdout (for a hand-merge)")
	return c
}

func curatorAcceptCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "accept <id> [vault]",
		Short: "Acknowledge a curator merge that is already in your vault (no-op)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseJobID(args[0])
			if err != nil {
				return err
			}
			vaultDir := "."
			if len(args) == 2 {
				vaultDir = args[1]
			}
			cl, err := meshclient.ClientForVault(vaultDir)
			if err != nil {
				return err
			}
			job, err := cl.CurationJob(id)
			if err != nil {
				return err
			}
			if job.Status == "failed" {
				return fmt.Errorf("job %d failed; there is nothing to accept. See: mesh curator show %d", id, id)
			}
			fmt.Printf("job %d merged %s; this merge is already in your vault (delivered by sync). Nothing to do.\n", job.ID, textdiff.Sanitize(job.Path))
			return nil
		},
	}
}

// safeRelPath reports whether a hub-supplied note path is safe to join to the
// local vault and read: vault-relative (no absolute, no ".." escape) and not in a
// reserved directory. Mirrors the curator daemon's own write-boundary guard.
func safeRelPath(p string) bool {
	clean := filepath.FromSlash(p)
	if !filepath.IsLocal(clean) {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if part == ".git" || part == ".mesh" {
			return false
		}
	}
	return true
}

func parseJobID(arg string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(arg), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid job id %q", arg)
	}
	return id, nil
}

func whenStr(unix int64) string {
	if unix <= 0 {
		return "unknown"
	}
	return time.Unix(unix, 0).Format("2006-01-02 15:04")
}
