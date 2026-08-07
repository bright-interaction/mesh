// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/merge"
	"github.com/bright-interaction/mesh/internal/syncproto"
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
			want, err := parseCuratorStatusFilter(statusFilter)
			if err != nil {
				return err
			}
			jobs, scan, err := collectCuratorLog(cl.CurationActivity, limit, want)
			if err != nil {
				return err
			}
			if asJSON {
				b, _ := json.MarshalIndent(jobs, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			if len(jobs) == 0 {
				for _, line := range curatorLogEmptyLines(want, scan) {
					fmt.Println(line)
				}
				return nil
			}
			if len(want) > 0 {
				fmt.Printf("%d matching curator job(s) of %d scanned, newest first:\n", len(jobs), scan.Scanned)
			} else {
				fmt.Printf("%d curator job(s), newest first:\n", len(jobs))
			}
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
			if scan.Capped {
				fmt.Printf("  the scan stopped at the hub's %d-row ceiling for one read, so older jobs may exist below it.\n", curatorLogScanCap)
			}
			return nil
		},
	}
	c.Flags().IntVar(&limit, "limit", 50, "max MATCHING jobs to list (the hub caps one read at 200 rows)")
	c.Flags().StringVar(&statusFilter, "status", "", "comma-separated filter: resolved,failed")
	c.Flags().BoolVar(&asJSON, "json", false, "emit jobs as JSON")
	return c
}

const (
	// curatorLogScanCap bounds how many raw activity rows one `curator log` may pull.
	// The hub clamps ?limit to 200 and its activity endpoint exposes no cursor, so 200
	// rows is everything a single client read can reach: this is the hub's own ceiling,
	// not a second softer one invented here.
	curatorLogScanCap = 200
	// curatorLogFirstPage is the smallest page a FILTERED scan asks for. An unfiltered
	// read still asks for exactly what the user requested.
	curatorLogFirstPage = 50
	// curatorLogGrowth multiplies the page each round. With the two constants above a
	// filtered scan costs at most two requests (50 then 200), which is what keeps this
	// bounded rather than "page until you find something".
	curatorLogGrowth = 4
)

// curatorLogScan records how far a scan actually got, so the caller can be honest about
// what an empty result does and does not prove.
type curatorLogScan struct {
	Scanned int  // raw rows examined
	Capped  bool // stopped short with rows possibly still below the last page
}

// collectCuratorLog fetches curator activity and applies the --status filter, paging
// until it holds `limit` MATCHING rows.
//
// The endpoint returns the N most recent terminal jobs and takes no status parameter,
// so filtering the response of one fixed fetch repeats the LIMIT-before-filter mistake
// the hub store just fixed one layer down: `mesh curator log --status failed` printed
// "no curator activity yet." whenever the newest N jobs had all merged cleanly, while
// the failed jobs the operator came for sat one row below the page. Failures are the
// rarer status by design, so they are exactly the ones that fall off it.
//
// Growth is bounded by curatorLogScanCap (the hub's own clamp), so a filtered read
// costs at most two requests and 200 metadata rows no matter how long the history is.
func collectCuratorLog(fetch func(int) ([]syncproto.CurationJob, error), limit int, want map[string]bool) ([]syncproto.CurationJob, curatorLogScan, error) {
	if limit < 1 {
		limit = 1
	}
	size := limit
	if len(want) > 0 && size < curatorLogFirstPage {
		size = curatorLogFirstPage
	}
	for {
		size = min(size, curatorLogScanCap)
		raw, err := fetch(size)
		if err != nil {
			return nil, curatorLogScan{}, err
		}
		matched := make([]syncproto.CurationJob, 0, min(len(raw), limit))
		for _, j := range raw {
			if len(matched) == limit {
				break
			}
			if len(want) > 0 && !want[j.Status] {
				continue
			}
			matched = append(matched, j)
		}
		// A short page means the history ended inside it, so there is nothing below to
		// find and a bigger request would return the same rows.
		full := len(raw) >= size
		if len(matched) == limit || !full || size == curatorLogScanCap {
			return matched, curatorLogScan{Scanned: len(raw), Capped: full && len(matched) < limit}, nil
		}
		size *= curatorLogGrowth
	}
}

// curatorTerminalStatuses are the only statuses the activity endpoint can return; it
// lists terminal jobs only. Validating against it turns a typo (`--status Failed`, or
// `--status pending`) into an error instead of a confident empty page, which is the
// same wrong conclusion the paging fix above exists to prevent.
var curatorTerminalStatuses = []string{"failed", "resolved"}

func parseCuratorStatusFilter(raw string) (map[string]bool, error) {
	want := map[string]bool{}
	for _, s := range strings.Split(raw, ",") {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if !slices.Contains(curatorTerminalStatuses, s) {
			return nil, fmt.Errorf("unknown --status %q; the activity log holds terminal jobs only (%s)",
				s, strings.Join(curatorTerminalStatuses, ", "))
		}
		want[s] = true
	}
	if len(want) == 0 {
		return nil, nil
	}
	return want, nil
}

// curatorLogEmptyLines is the honest empty state. "no curator activity yet." used to be
// printed for two different facts: an empty history, and a filter that matched nothing
// in the rows that were scanned. The second one reads as "the curator has never failed",
// which is the opposite of what a scan that fell short actually establishes.
func curatorLogEmptyLines(want map[string]bool, scan curatorLogScan) []string {
	if len(want) == 0 || scan.Scanned == 0 {
		return []string{"no curator activity yet."}
	}
	lines := []string{fmt.Sprintf("no curator job with status %s in the %d most recent job(s) scanned; that is not the same as no such job.",
		strings.Join(slices.Sorted(maps.Keys(want)), ","), scan.Scanned)}
	if scan.Capped {
		lines = append(lines, fmt.Sprintf("  the scan stopped at the hub's %d-row ceiling for one read, so older matching jobs may exist below it.", curatorLogScanCap))
	}
	return lines
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
