package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/bright-interaction/mesh/internal/extract"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/llm"
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
	var toPending string
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
				return runExtractBenchmark(cmd.Context(), client, args[0], n, concurrency, maxChars, asJSON)
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
	return c
}

// writeToPending opens the vault index and stores extracted candidates in the review
// queue (pending_notes), idempotently. Used by the Stop hook's auto-extraction.
func writeToPending(vaultRoot, transcript string, cands []extract.Candidate) error {
	store, err := index.Open(vaultRoot)
	if err != nil {
		return err
	}
	defer store.Close()
	src := filepath.Base(transcript)
	for _, cnd := range cands {
		if err := store.AddPending(index.PendingNote{
			Type: cnd.Type, Title: cnd.Title, Do: cnd.Do, Dont: cnd.Dont,
			Why: cnd.Why, Confidence: cnd.Confidence, Source: src,
		}); err != nil {
			return err
		}
	}
	fmt.Printf("queued %d candidate(s) for review in %s\n", len(cands), vaultRoot)
	return nil
}

type benchRow struct {
	File         string `json:"file"`
	SizeKB       int64  `json:"size_kb"`
	HadWriteback bool   `json:"had_writeback"`
	Candidates   int    `json:"candidates"`
	Kept         int    `json:"kept"`
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

func runExtractBenchmark(ctx context.Context, client llm.Client, dir string, n, concurrency, maxChars int, asJSON bool) error {
	files, err := sampleTranscripts(dir, n)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no usable transcripts in %s", dir)
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
				fmt.Fprintf(os.Stderr, "  %-40s writeback=%-5v candidates=%d kept=%d %s\n", row.File, row.HadWriteback, row.Candidates, row.Kept, row.Err)
			}
		}(i, f)
	}
	wg.Wait()

	// Tally.
	var manual, extractCov, totalC, totalK, errs int
	for _, r := range rows {
		if r.Err != "" && r.Candidates == 0 {
			errs++
		}
		if r.HadWriteback {
			manual++
		}
		if r.Candidates > 0 {
			extractCov++
		}
		totalC += r.Candidates
		totalK += r.Kept
	}
	N := len(rows)
	precision := 0.0
	if totalC > 0 {
		precision = float64(totalK) / float64(totalC) * 100
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
			"candidates": totalC, "kept": totalK, "precision_pct": precision,
			"verdict": verdict, "rows": rows,
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
	fmt.Printf("  sessions yielding >=1 candidate: %d / %d   (%.0f%% coverage)\n", extractCov, N, pct(extractCov, N))
	fmt.Printf("  candidate notes:                 %d   (%.1f per session)\n", totalC, func() float64 { return float64(totalC) / float64(max1(N)) }())
	fmt.Printf("  judged keep-worthy:              %d / %d   (PRECISION %.0f%%)\n", totalK, totalC, precision)
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
