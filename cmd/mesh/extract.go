// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/bright-interaction/mesh/internal/extract"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/llm"
	"github.com/bright-interaction/mesh/internal/retrieve"
	"github.com/spf13/cobra"
)

// extractCmd pulls candidate write-back notes from a finished session transcript (the
// input side of the flywheel), and --benchmark measures it against the current manual
// algo (the Stop-hook nudge): coverage (sessions that yield a note) and precision (how
// many extracted notes a strict judge would keep). BYOAI via MESH_CURATOR_* (claude -p).
func extractCmd() *cobra.Command {
	var benchmark bool
	var n, concurrency, maxChars int
	var asJSON bool
	var toPending, dedupVault string
	var recurring bool
	c := &cobra.Command{
		Use:   "extract [transcript.jsonl]",
		Short: "Extract candidate write-back notes from a session transcript (--benchmark to grade vs manual write-back)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := llm.NewFromEnv()
			if err != nil {
				return err
			}
			if benchmark {
				if len(args) < 1 {
					return fmt.Errorf("--benchmark needs a directory of transcripts")
				}
				return runExtractBenchmark(cmd.Context(), client, args[0], n, concurrency, maxChars, asJSON, dedupVault)
			}
			if recurring {
				if len(args) < 1 {
					return fmt.Errorf("--recurring needs a directory of transcripts")
				}
				return runRecurring(cmd.Context(), client, args[0], n, concurrency, maxChars, asJSON)
			}
			if len(args) < 1 {
				return fmt.Errorf("usage: mesh extract <transcript.jsonl>  (or --benchmark <dir>)")
			}
			digest, st, err := extract.Digest(args[0], maxChars)
			if err != nil {
				return err
			}
			cands, err := extract.Extract(cmd.Context(), client, digest)
			if err != nil {
				return err
			}
			if toPending != "" {
				return writeToPending(toPending, args[0], cands)
			}
			b, _ := json.MarshalIndent(map[string]any{"stats": st, "candidates": cands}, "", "  ")
			fmt.Println(string(b))
			return nil
		},
	}
	c.Flags().BoolVar(&benchmark, "benchmark", false, "grade the extractor over a directory of transcripts vs manual write-back")
	c.Flags().IntVar(&n, "n", 15, "benchmark: max transcripts to sample")
	c.Flags().IntVar(&concurrency, "concurrency", 4, "benchmark: parallel LLM calls")
	c.Flags().IntVar(&maxChars, "max-chars", 48000, "max digest size fed to the model")
	c.Flags().BoolVar(&asJSON, "json", false, "benchmark: emit JSON instead of a report")
	c.Flags().StringVar(&toPending, "to-pending", "", "persist extracted candidates to this vault's review queue instead of printing")
	c.Flags().StringVar(&dedupVault, "dedup-vault", "", "benchmark: dedup candidates against this vault and report fresh vs already-known")
	c.Flags().BoolVar(&recurring, "recurring", false, "find problems that RECUR across sessions (systemic issues), not one-offs")
	return c
}

// runRecurring extracts candidates from many transcripts and clusters them by
// similarity to surface SYSTEMIC issues: a learning that recurs across multiple
// sessions is worth a permanent fix, not just another write-back.
func runRecurring(ctx context.Context, client llm.Client, dir string, n, concurrency, maxChars int, asJSON bool) error {
	files, err := sampleTranscripts(dir, n)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no usable transcripts in %s", dir)
	}
	if concurrency < 1 {
		concurrency = 4
	}
	if !asJSON {
		fmt.Fprintf(os.Stderr, "extracting from %d transcripts via %s...\n", len(files), client.Describe())
	}
	var mu sync.Mutex
	var occs []extract.Occurrence
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, f := range files {
		wg.Add(1)
		go func(f string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			digest, _, e := extract.Digest(f, maxChars)
			if e != nil {
				return
			}
			cands, e := extract.Extract(ctx, client, digest)
			if e != nil {
				return
			}
			sess := filepath.Base(f)
			mu.Lock()
			for _, c := range cands {
				occs = append(occs, extract.Occurrence{Cand: c, Session: sess})
			}
			mu.Unlock()
		}(f)
	}
	wg.Wait()

	clusters := extract.ClusterRecurring(occs, extract.DuplicateThreshold)
	var recurringClusters []extract.Cluster
	for _, c := range clusters {
		if c.Count >= 2 {
			recurringClusters = append(recurringClusters, c)
		}
	}
	if asJSON {
		b, _ := json.MarshalIndent(map[string]any{
			"sessions": len(files), "candidates": len(occs),
			"recurring": recurringClusters,
		}, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	fmt.Printf("\nRecurring problems across %d sessions (%d candidates extracted):\n", len(files), len(occs))
	if len(recurringClusters) == 0 {
		fmt.Println("  none yet: no learning recurred across 2+ sessions in this sample.")
		return nil
	}
	for _, c := range recurringClusters {
		fmt.Printf("\n* [%s] %s\n  recurred in %d sessions; a permanent fix beats re-learning it:\n", c.Rep.Type, c.Rep.Title, c.Count)
		seen := map[string]bool{}
		for _, m := range c.Members {
			if seen[m.Title] {
				continue
			}
			seen[m.Title] = true
			fmt.Printf("    - %s\n", m.Title)
		}
	}
	return nil
}

// writeToPending opens the vault index and stores extracted candidates in the review
// queue (pending_notes), skipping any that restate a note already in the vault (so the
// queue surfaces only NEW knowledge). Idempotent. Used by the Stop hook's extraction.
func writeToPending(vaultRoot, transcript string, cands []extract.Candidate) error {
	store, err := index.Open(vaultRoot)
	if err != nil {
		return err
	}
	defer store.Close()
	rtr, closeRtr := buildVaultRetriever(store, vaultRoot) // best-effort dedup context
	defer closeRtr()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Review items already queued, so a candidate that restates one (from an earlier run
	// or another session) is suppressed. The exact-title PendingID is not enough: the LLM
	// rephrases the same learning differently each run, so two reruns hash to different
	// ids and would both pile up. Compare by title similarity, the same bar used against
	// the vault, so the queue cannot flood with reworded duplicates.
	queuedTitles, _ := store.ListPending()

	src := filepath.Base(transcript)
	queued, dupes := 0, 0
	for _, cnd := range cands {
		if known, of := knownInVault(ctx, rtr, cnd); known {
			dupes++
			fmt.Printf("skip (already known): %q ~ %q\n", cnd.Title, of)
			continue
		}
		if of, dup := nearDuplicatePending(cnd.Title, queuedTitles); dup {
			dupes++
			fmt.Printf("skip (already queued): %q ~ %q\n", cnd.Title, of)
			continue
		}
		if err := store.AddPending(index.PendingNote{
			Type: cnd.Type, Title: cnd.Title, Do: cnd.Do, Dont: cnd.Dont,
			Why: cnd.Why, Confidence: cnd.Confidence, Source: src,
		}); err != nil {
			return err
		}
		// Track it so two near-duplicate candidates in the SAME batch also collapse.
		queuedTitles = append(queuedTitles, index.PendingNote{Type: cnd.Type, Title: cnd.Title})
		queued++
	}
	fmt.Printf("queued %d candidate(s) for review in %s (%d skipped as duplicate)\n", queued, vaultRoot, dupes)
	return nil
}

// nearDuplicatePending reports whether a candidate title restates a review item already
// in the queue, by the same title-similarity bar used to dedup against the vault. This
// stops the queue from filling with the LLM's slightly reworded reruns of one learning.
func nearDuplicatePending(title string, existing []index.PendingNote) (string, bool) {
	for _, p := range existing {
		if extract.TitleSimilarity(title, p.Title) >= extract.DuplicateThreshold {
			return p.Title, true
		}
	}
	return "", false
}

// buildVaultRetriever builds a retriever over the vault for dedup. Best-effort: any
// failure returns a nil retriever (dedup is then skipped, candidates all queue).
func buildVaultRetriever(store *index.Store, vaultRoot string) (*retrieve.Retriever, func()) {
	if _, err := index.Reconcile(store, vaultRoot); err != nil {
		fmt.Fprintf(os.Stderr, "dedup disabled (reindex %s: %v)\n", vaultRoot, err)
		return nil, func() {}
	}
	g, err := store.LoadGraph()
	if err != nil {
		fmt.Fprintf(os.Stderr, "dedup disabled (load graph: %v)\n", err)
		return nil, func() {}
	}
	return retrieve.NewFromEnv(store, g), func() {}
}

// knownInVault reports whether a candidate restates a note already in the vault, by
// retrieving its topical neighbors and comparing titles. Returns the matched title.
func knownInVault(ctx context.Context, rtr *retrieve.Retriever, c extract.Candidate) (bool, string) {
	if rtr == nil {
		return false, ""
	}
	cards, err := rtr.Retrieve(ctx, c.Title+" "+c.Do, retrieve.Options{Limit: 5})
	if err != nil {
		return false, ""
	}
	for _, card := range cards {
		if extract.TitleSimilarity(c.Title, card.Title) >= extract.DuplicateThreshold {
			return true, card.Title
		}
	}
	return false, ""
}

type benchRow struct {
	File         string `json:"file"`
	SizeKB       int64  `json:"size_kb"`
	HadWriteback bool   `json:"had_writeback"`
	Candidates   int    `json:"candidates"`
	Duplicates   int    `json:"duplicates"` // candidates that restate an existing vault note (dedup)
	Kept         int    `json:"kept"`       // fresh candidates a strict judge would keep
	Err          string `json:"err,omitempty"`
}

// sampleTranscripts picks up to n transcripts to grade: skip trivially small ones
// (<50KB, no real work) and skip the huge in-progress current session (>40MB, also too
// slow to be representative), then take the largest remaining (most substantive work).
func sampleTranscripts(dir string, n int) ([]string, error) {
	all, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	type fi struct {
		path string
		size int64
	}
	var fis []fi
	for _, p := range all {
		st, e := os.Stat(p)
		if e != nil {
			continue
		}
		if st.Size() < 50<<10 || st.Size() > 40<<20 {
			continue
		}
		fis = append(fis, fi{p, st.Size()})
	}
	sort.Slice(fis, func(i, j int) bool { return fis[i].size > fis[j].size })
	if n > 0 && len(fis) > n {
		fis = fis[:n]
	}
	out := make([]string, len(fis))
	for i, f := range fis {
		out[i] = f.path
	}
	return out, nil
}

func runExtractBenchmark(ctx context.Context, client llm.Client, dir string, n, concurrency, maxChars int, asJSON bool, dedupVault string) error {
	files, err := sampleTranscripts(dir, n)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no usable transcripts in %s", dir)
	}
	// Optional dedup context: a retriever over an existing vault, so a candidate that
	// restates a note already there is counted separately from genuinely fresh ones.
	var rtr *retrieve.Retriever
	if dedupVault != "" {
		if vs, e := index.Open(dedupVault); e == nil {
			defer vs.Close()
			rtr, _ = buildVaultRetriever(vs, dedupVault)
		}
	}
	if !asJSON {
		fmt.Fprintf(os.Stderr, "grading %d transcripts via %s (this calls the LLM per transcript)...\n", len(files), client.Describe())
	}
	if concurrency < 1 {
		concurrency = 1
	}
	rows := make([]benchRow, len(files))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, f := range files {
		wg.Add(1)
		go func(i int, f string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			row := benchRow{File: filepath.Base(f)}
			if st, e := os.Stat(f); e == nil {
				row.SizeKB = st.Size() >> 10
			}
			digest, st, e := extract.Digest(f, maxChars)
			if e != nil {
				row.Err = "digest: " + e.Error()
				rows[i] = row
				return
			}
			row.HadWriteback = st.HadWriteback
			cands, e := extract.Extract(ctx, client, digest)
			if e != nil {
				row.Err = "extract: " + e.Error()
				rows[i] = row
				return
			}
			row.Candidates = len(cands)
			for _, cnd := range cands {
				if known, _ := knownInVault(ctx, rtr, cnd); known {
					row.Duplicates++ // restates a note already in the vault; not judged as fresh
					continue
				}
				keep, _, je := extract.Judge(ctx, client, cnd)
				if je != nil {
					row.Err = "judge: " + je.Error()
					continue
				}
				if keep {
					row.Kept++
				}
			}
			rows[i] = row
			if !asJSON {
				fmt.Fprintf(os.Stderr, "  %-40s writeback=%-5v cands=%d dup=%d kept=%d %s\n", row.File, row.HadWriteback, row.Candidates, row.Duplicates, row.Kept, row.Err)
			}
		}(i, f)
	}
	wg.Wait()

	// Tally. "Fresh" = candidates that are not duplicates of an existing vault note;
	// precision is measured on the fresh set (what a reviewer actually sees).
	var manual, extractCov, totalC, totalDup, totalK, errs int
	for _, r := range rows {
		if r.Err != "" && r.Candidates == 0 {
			errs++
		}
		if r.HadWriteback {
			manual++
		}
		if r.Candidates-r.Duplicates > 0 {
			extractCov++
		}
		totalC += r.Candidates
		totalDup += r.Duplicates
		totalK += r.Kept
	}
	N := len(rows)
	fresh := totalC - totalDup
	precision := 0.0
	if fresh > 0 {
		precision = float64(totalK) / float64(fresh) * 100
	}
	verdict := "STOP (precision < 60%: the review queue would be a spam filter)"
	switch {
	case precision >= 80:
		verdict = "SHIP the pending-review queue (precision >= 80%)"
	case precision >= 60:
		verdict = "SHIP behind review (precision 60-80%: human veto carries it)"
	}

	if asJSON {
		b, _ := json.MarshalIndent(map[string]any{
			"sampled": N, "errors": errs,
			"manual_writeback_sessions": manual, "extraction_coverage_sessions": extractCov,
			"candidates": totalC, "duplicates": totalDup, "fresh": fresh, "kept": totalK,
			"precision_pct": precision, "verdict": verdict, "rows": rows,
		}, "", "  ")
		fmt.Println(string(b))
		return nil
	}

	pct := func(a, b int) float64 {
		if b == 0 {
			return 0
		}
		return float64(a) / float64(b) * 100
	}
	fmt.Printf("\nMesh write-back benchmark: auto-extraction vs the current manual (nudge) algo\n")
	fmt.Printf("transcripts graded: %d   (LLM errors: %d)   dir: %s\n", N, errs, dir)
	fmt.Printf("\nCURRENT ALGO (manual write-back, Stop-hook nudge)\n")
	fmt.Printf("  sessions that wrote back:        %d / %d   (%.0f%% coverage)\n", manual, N, pct(manual, N))
	fmt.Printf("\nAUTO-EXTRACTION\n")
	fmt.Printf("  sessions yielding >=1 fresh note: %d / %d   (%.0f%% coverage)\n", extractCov, N, pct(extractCov, N))
	fmt.Printf("  candidate notes:                 %d   (%.1f per session)\n", totalC, float64(totalC)/float64(max1(N)))
	if dedupVault != "" {
		fmt.Printf("  already-known (deduped out):     %d   (%.0f%% of candidates)\n", totalDup, pct(totalDup, max1(totalC)))
		fmt.Printf("  fresh (reach review):            %d\n", fresh)
	}
	fmt.Printf("  judged keep-worthy (of fresh):   %d / %d   (PRECISION %.0f%%)\n", totalK, fresh, precision)
	lift := "n/a"
	if manual > 0 {
		lift = fmt.Sprintf("%.1fx", pct(extractCov, N)/pct(manual, N))
	} else if extractCov > 0 {
		lift = "from zero"
	}
	fmt.Printf("\nCOVERAGE LIFT: %s (%.0f%% of sessions get a note vs %.0f%% manually)\n", lift, pct(extractCov, N), pct(manual, N))
	fmt.Printf("VERDICT: %s\n", verdict)
	return nil
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}
