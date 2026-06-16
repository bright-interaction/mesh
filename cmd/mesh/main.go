package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/bright-interaction/mesh/internal/embed"
	"github.com/bright-interaction/mesh/internal/eval"
	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/mcp"
	"github.com/bright-interaction/mesh/internal/retrieve"
	"github.com/bright-interaction/mesh/internal/vault"
	"github.com/spf13/cobra"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "mesh",
		Short:         "Mesh: a sovereign knowledge mesh built for coding agents",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.AddCommand(
		initCmd(),
		newCmd(),
		indexCmd(),
		embedCmd(),
		searchCmd(),
		evalCmd(),
		tuneCmd(),
		statusCmd(),
		migrateCmd(),
		lintCmd(),
		mcpCmd(),
		tuiCmd(),
		uiCmd(),
		doctorCmd(),
	)
	return root
}

func initCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "init [path]",
		Short: "Bootstrap a new Mesh vault (starter index + first build)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := vaultArg(args)
			if err := os.MkdirAll(root, 0o755); err != nil {
				return err
			}
			files, err := vault.Walk(root)
			if err != nil {
				return err
			}
			if len(files) == 0 {
				date := vault.Now().Format("2006-01-02")
				starter := "---\nid: index\ntype: map\ntitle: Vault index\nwhen: \"" + date + "\"\n---\n\n" +
					"# Vault index\n\nA Mesh vault. Add notes with `mesh new <type> \"<title>\"`, then `mesh index`.\n"
				if err := os.WriteFile(filepath.Join(root, "index.md"), []byte(starter), 0o644); err != nil {
					return err
				}
			}
			store, err := index.Open(root)
			if err != nil {
				return err
			}
			defer store.Close()
			g, err := index.Reindex(store, root)
			if err != nil {
				return err
			}
			abs, _ := filepath.Abs(root)
			fmt.Printf("initialized Mesh vault at %s (%d notes, %d nodes, %d edges)\n", root, g.CountByKind()["note"], g.NodeCount(), g.EdgeCount())
			fmt.Println("next:")
			fmt.Println("  mesh new decision \"<title>\" --vault " + root + "   # capture a decision/gotcha")
			fmt.Println("  mesh index " + root + "                          # rebuild after edits")
			fmt.Println("  point your coding agent at the MCP server:")
			fmt.Printf("    {\"command\": \"mesh\", \"args\": [\"mcp\", \"--vault\", \"%s\"]}\n", abs)
			return nil
		},
	}
	return c
}

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor [vault]",
		Short: "Diagnose index freshness (drift), counts, and vault health",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := vaultArg(args)
			dbPath := filepath.Join(root, ".mesh", "mesh.db")
			if _, err := os.Stat(dbPath); err != nil {
				fmt.Printf("no index at %s\n  fix: mesh index %s\n", dbPath, root)
				return fmt.Errorf("no index")
			}
			store, err := index.Open(root)
			if err != nil {
				return err
			}
			defer store.Close()

			notes, _ := store.Count("notes")
			nodes, _ := store.Count("nodes")
			edges, _ := store.Count("edges")
			fmt.Printf("index:  %s\n  notes %d  nodes %d  edges %d\n", dbPath, notes, nodes, edges)

			drift, err := store.DriftReport(root)
			if err != nil {
				return err
			}
			fmt.Printf("drift:  +%d new  ~%d changed  -%d removed\n", len(drift.Added), len(drift.Changed), len(drift.Removed))

			files, _ := vault.Walk(root)
			parsed, _ := index.ParseFiles(files, 0)
			_, issues := index.BuildGraph(parsed)
			lintProblems := 0
			for _, pn := range parsed {
				for _, e := range pn.FM.Validate() {
					if e != "missing id" {
						lintProblems++
					}
				}
			}
			lintProblems += len(issues)
			fmt.Printf("lint:   %d problems (run mesh lint for detail)\n", lintProblems)

			switch {
			case drift.Any():
				fmt.Println("status: STALE - run mesh index")
				return fmt.Errorf("index stale")
			case lintProblems > 0:
				fmt.Println("status: OK (index fresh; lint problems exist)")
			default:
				fmt.Println("status: healthy")
			}
			return nil
		},
	}
}

func searchCmd() *cobra.Command {
	var vaultDir string
	var limit, budget int
	c := &cobra.Command{
		Use:   "search <query>",
		Short: "Fused retrieval over the indexed vault (FTS + graph, tier-0 boosted, budget-packed)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dbPath := filepath.Join(vaultDir, ".mesh", "mesh.db")
			if _, err := os.Stat(dbPath); err != nil {
				return fmt.Errorf("no index at %s (run: mesh index %s)", dbPath, vaultDir)
			}
			store, err := index.Open(vaultDir)
			if err != nil {
				return err
			}
			defer store.Close()
			g, err := store.LoadGraph()
			if err != nil {
				return err
			}
			cards, err := retrieve.NewFromEnv(store, g).Retrieve(strings.Join(args, " "), retrieve.Options{Limit: limit, Budget: budget})
			if err != nil {
				return err
			}
			if len(cards) == 0 {
				fmt.Println("no matches")
				return nil
			}
			for i, c := range cards {
				tier := ""
				if c.Tier0 {
					tier = " [tier-0]"
				}
				fmt.Printf("%d. %s%s  (%s)\n", i+1, c.Title, tier, c.Path)
				if sn := strings.TrimSpace(c.Snippet); sn != "" {
					fmt.Printf("   %s\n", sn)
				}
				if c.Reason != "" {
					fmt.Printf("   ~ %s\n", c.Reason)
				}
			}
			if budget > 0 {
				fmt.Printf("packed %d cards, ~%d tokens (budget %d)\n", len(cards), retrieve.TotalTokens(cards), budget)
			}
			return nil
		},
	}
	c.Flags().StringVar(&vaultDir, "vault", ".", "vault root")
	c.Flags().IntVar(&limit, "limit", 20, "candidates per signal")
	c.Flags().IntVar(&budget, "budget", 0, "token budget for packing (0 = all ranked)")
	return c
}

func evalCmd() *cobra.Command {
	var vaultDir, casesFile string
	var budget int
	c := &cobra.Command{
		Use:   "eval <cases.json>",
		Short: "Gate 1: measure Mesh retrieval vs the read-top-3-FTS baseline on a labelled query set",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				casesFile = args[0]
			}
			if casesFile == "" {
				return fmt.Errorf("provide a cases file: mesh eval <cases.json> --vault <dir>")
			}
			raw, err := os.ReadFile(casesFile)
			if err != nil {
				return err
			}
			var cases []eval.Case
			if err := json.Unmarshal(raw, &cases); err != nil {
				return fmt.Errorf("parse cases: %w", err)
			}
			store, err := index.Open(vaultDir)
			if err != nil {
				return err
			}
			defer store.Close()
			g, err := store.LoadGraph()
			if err != nil {
				return err
			}
			rep := eval.RunGate(store, retrieve.NewFromEnv(store, g), vaultDir, cases, budget)

			pf := func(b bool) string {
				if b {
					return "PASS"
				}
				return "FAIL"
			}
			fmt.Printf("Gate 1: Mesh vs FTS baselines  (vault: %s, %d cases, budget %d, tokenizer: estimate)\n", vaultDir, rep.N, budget)
			fmt.Printf("  surfacing recall @K=%d:   mesh %d/%d   fts %d/%d\n", 20, rep.MeshSurfaced, rep.N, rep.FTSSurfaced, rep.N)
			fmt.Printf("  answer@1 (one body read): mesh %d/%d   fts-top1 %d/%d\n", rep.MeshAnswer1, rep.N, rep.FTSAnswer1, rep.N)
			fmt.Printf("  tokens median:  mesh %.0f   fts-top1 %.0f (matched)   fts-top3 %.0f (naive)\n", rep.MeshMedian, rep.FTSTop1Median, rep.FTSTop3Median)
			fmt.Printf("  tokens mean:    mesh %.0f   fts-top1 %.0f             fts-top3 %.0f\n", rep.MeshMean, rep.FTSTop1Mean, rep.FTSTop3Mean)
			fmt.Printf("  sub-claims: surfacing>=fts %s | answer@1>=fts-top1 %s | cheaper-than-naive-top3 %s\n",
				pf(rep.SurfacingWin), pf(rep.AnswerWin), pf(rep.NaiveCostWin))
			if rep.Pass {
				fmt.Println("  VERDICT: PASS (all three sub-claims hold)")
				return nil
			}
			fmt.Println("  VERDICT: PARTIAL (see sub-claims; matched fts-top1 cost shows the card overhead honestly)")
			return fmt.Errorf("gate 1 not fully met")
		},
	}
	c.Flags().StringVar(&vaultDir, "vault", ".", "vault root")
	c.Flags().IntVar(&budget, "budget", 0, "token budget for the Mesh arm (0 = unbudgeted)")
	return c
}

func loadCases(files ...string) ([]eval.Case, error) {
	var all []eval.Case
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		var cs []eval.Case
		if err := json.Unmarshal(raw, &cs); err != nil {
			return nil, fmt.Errorf("parse %s: %w", f, err)
		}
		all = append(all, cs...)
	}
	return all, nil
}

func tuneCmd() *cobra.Command {
	var vaultDir, testFile string
	var step, holdout float64
	c := &cobra.Command{
		Use:   "tune <train-cases.json> [more-cases.json ...]",
		Short: "Learn fusion weights (FTS/graph/vector) from labelled queries, validated on held-out",
		Long:  "Grid-searches the fusion-weight simplex to maximize answer@1 on the training queries (rerank off, so the fused order is what is measured), then reports how the learned weights and the built-in defaults score on a held-out test split. Set the winner with MESH_WEIGHT_FTS/GRAPH/VEC. Tuning to the same queries you report on is p-hacking; pass --test (or use --holdout) so the headline number is held-out.",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			train, err := loadCases(args...)
			if err != nil {
				return err
			}
			var test []eval.Case
			if testFile != "" {
				if test, err = loadCases(testFile); err != nil {
					return err
				}
			} else {
				// Deterministic interleave split so the headline is still held-out
				// without a separate file: every k-th case goes to test.
				if holdout <= 0 || holdout >= 1 {
					holdout = 0.33
				}
				k := int(math.Round(1 / holdout))
				if k < 2 {
					k = 2
				}
				var tr []eval.Case
				for i, cse := range train {
					if (i+1)%k == 0 {
						test = append(test, cse)
					} else {
						tr = append(tr, cse)
					}
				}
				train = tr
			}
			if len(train) == 0 || len(test) == 0 {
				return fmt.Errorf("need non-empty train and test sets (train %d, test %d)", len(train), len(test))
			}
			store, err := index.Open(vaultDir)
			if err != nil {
				return err
			}
			defer store.Close()
			g, err := store.LoadGraph()
			if err != nil {
				return err
			}
			r := retrieve.NewFromEnv(store, g)
			vectors := r.VectorsActive()
			if step <= 0 || step > 0.5 {
				step = 0.05
			}
			rep := eval.TuneWeights(r, train, test, step, vectors)

			fmt.Printf("mesh tune (vault %s, vectors %v, %d candidates, step %.2f)\n", vaultDir, vectors, rep.Candidates, step)
			fmt.Printf("  train %d cases, test %d cases (held-out)\n", len(train), len(test))
			w := func(s eval.WeightSet) string {
				return fmt.Sprintf("fts=%.2f graph=%.2f vec=%.2f", s.FTS, s.Graph, s.Vec)
			}
			sc := func(s eval.Score) string {
				return fmt.Sprintf("answer@1 %d/%d, recall %d/%d", s.Answer1, s.N, s.Recall, s.N)
			}
			fmt.Printf("  default (%s):\n      train %s | test %s\n", w(rep.Default), sc(rep.DefaultTrain), sc(rep.DefaultTest))
			fmt.Printf("  learned (%s):\n      train %s | test %s\n", w(rep.Best), sc(rep.BestTrain), sc(rep.BestTest))
			win := rep.BestTest.Answer1 > rep.DefaultTest.Answer1
			tie := rep.BestTest.Answer1 == rep.DefaultTest.Answer1
			switch {
			case win:
				fmt.Printf("  VERDICT: learned weights beat default on held-out (+%d answer@1). Apply with:\n", rep.BestTest.Answer1-rep.DefaultTest.Answer1)
				fmt.Printf("      export MESH_WEIGHT_FTS=%.2f MESH_WEIGHT_GRAPH=%.2f MESH_WEIGHT_VEC=%.2f\n", rep.Best.FTS, rep.Best.Graph, rep.Best.Vec)
			case tie:
				fmt.Println("  VERDICT: learned weights TIE the default on held-out; keep the default (no evidence of a real gain).")
			default:
				fmt.Printf("  VERDICT: learned weights LOSE on held-out (%d vs %d answer@1); the train win did not generalize. Keep the default.\n", rep.BestTest.Answer1, rep.DefaultTest.Answer1)
			}
			return nil
		},
	}
	c.Flags().StringVar(&vaultDir, "vault", ".", "vault root")
	c.Flags().StringVar(&testFile, "test", "", "held-out test cases file (else --holdout splits the train set)")
	c.Flags().Float64Var(&step, "step", 0.05, "weight grid step (smaller = finer search)")
	c.Flags().Float64Var(&holdout, "holdout", 0.33, "test fraction when --test is not given")
	return c
}

func embedCmd() *cobra.Command {
	var endpoint, model, keyEnv string
	var batch int
	var perSection bool
	c := &cobra.Command{
		Use:   "embed [vault]",
		Short: "Embed notes via a BYOAI endpoint and store vectors (turns on semantic search)",
		Long:  "Calls an OpenAI-compatible /embeddings endpoint (Ollama, OpenAI, Voyage, ...) you control. Vectors stay in .mesh/mesh.db. After this, mesh search / eval / mcp fuse the semantic signal automatically.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := vaultArg(args)
			if endpoint == "" {
				endpoint = os.Getenv("MESH_EMBED_ENDPOINT")
			}
			if model == "" {
				model = os.Getenv("MESH_EMBED_MODEL")
			}
			if endpoint == "" || model == "" {
				return fmt.Errorf("set --endpoint and --model (or MESH_EMBED_ENDPOINT / MESH_EMBED_MODEL).\n  example: mesh embed %s --endpoint http://localhost:11434/v1 --model nomic-embed-text", root)
			}
			if _, err := os.Stat(filepath.Join(root, ".mesh", "mesh.db")); err != nil {
				return fmt.Errorf("no index (run: mesh index %s)", root)
			}
			store, err := index.Open(root)
			if err != nil {
				return err
			}
			defer store.Close()
			files, err := store.NoteFiles()
			if err != nil {
				return err
			}
			if len(files) == 0 {
				fmt.Println("no notes to embed")
				return nil
			}
			// Default: one vector per note (the structured title + flywheel + titled
			// sections joined). --per-section instead stores one vector per heading
			// section and scores a note by its best-matching section (max-pool). On
			// the Hive corpus per-section gave no recall or answer@1 lift at ~18x the
			// embedding cost, so whole-note is the default; the flag keeps the lever
			// available for long heterogeneous corpora where it may pay off.
			type chunkRef struct {
				NodeID  string
				ChunkIx int
				Text    string
			}
			var refs []chunkRef
			for _, nf := range files {
				pn, err := index.ParseFile(filepath.Join(root, nf.Path))
				if err != nil {
					return fmt.Errorf("parse %s: %w", nf.Path, err)
				}
				if !perSection {
					refs = append(refs, chunkRef{NodeID: nf.NodeID, ChunkIx: 0, Text: strings.Join(index.ChunkText(pn), "\n")})
					continue
				}
				for ix, text := range index.ChunkText(pn) {
					refs = append(refs, chunkRef{NodeID: nf.NodeID, ChunkIx: ix, Text: text})
				}
			}
			emb := embed.NewHTTP(endpoint, model, os.Getenv(keyEnv))
			if batch <= 0 {
				batch = 32
			}
			ctx := context.Background()
			docPrefix := os.Getenv("MESH_EMBED_DOC_PREFIX") // e.g. "search_document: " for nomic
			rows := make([]index.VectorRow, 0, len(refs))
			for i := 0; i < len(refs); i += batch {
				j := i + batch
				if j > len(refs) {
					j = len(refs)
				}
				inputs := make([]string, 0, j-i)
				for _, r := range refs[i:j] {
					inputs = append(inputs, docPrefix+r.Text)
				}
				vecs, err := emb.Embed(ctx, inputs)
				if err != nil {
					return fmt.Errorf("embed batch %d-%d via %s: %w", i, j, endpoint, err)
				}
				for k, v := range vecs {
					rows = append(rows, index.VectorRow{NodeID: refs[i+k].NodeID, ChunkIx: refs[i+k].ChunkIx, Vec: v})
				}
				fmt.Printf("\rembedded %d/%d chunks", j, len(refs))
			}
			fmt.Println()
			if err := store.ReplaceVectors(model, rows); err != nil {
				return err
			}
			dim := 0
			if len(rows) > 0 {
				dim = len(rows[0].Vec)
			}
			mode := "whole-note"
			if perSection {
				mode = "per-section"
			}
			fmt.Printf("stored %d vectors across %d notes (%s, model %s, dim %d); semantic search active for mesh search / eval / mcp\n", len(rows), len(files), mode, model, dim)
			return nil
		},
	}
	c.Flags().StringVar(&endpoint, "endpoint", "", "OpenAI-compatible embeddings base URL (or MESH_EMBED_ENDPOINT)")
	c.Flags().StringVar(&model, "model", "", "embedding model id (or MESH_EMBED_MODEL)")
	c.Flags().StringVar(&keyEnv, "key-env", "MESH_EMBED_KEY", "env var holding the bearer key (empty for local)")
	c.Flags().IntVar(&batch, "batch", 32, "embeddings per request")
	c.Flags().BoolVar(&perSection, "per-section", false, "store one vector per heading section instead of one per note (~18x more vectors; no measured lift on Hive)")
	return c
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status [vault]",
		Short: "Show index stats from .mesh/mesh.db",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := "."
			if len(args) == 1 {
				root = args[0]
			}
			dbPath := filepath.Join(root, ".mesh", "mesh.db")
			if _, err := os.Stat(dbPath); err != nil {
				return fmt.Errorf("no index at %s (run: mesh index %s)", dbPath, root)
			}
			store, err := index.Open(root)
			if err != nil {
				return err
			}
			defer store.Close()
			fmt.Printf("index:  %s\n", dbPath)
			for _, t := range []struct{ label, table string }{
				{"notes", "notes"},
				{"nodes", "nodes"},
				{"edges", "edges"},
				{"fts rows", "search_index"},
				{"vectors", "vectors"},
			} {
				n, err := store.Count(t.table)
				if err != nil {
					return err
				}
				fmt.Printf("  %-9s %d\n", t.label, n)
			}

			// Report which retrieval signals will actually fire, reflecting both the
			// stored index and the current BYOAI env config, so the operator can see
			// at a glance what mesh search / eval / mcp will use.
			g, err := store.LoadGraph()
			if err != nil {
				return err
			}
			r := retrieve.NewFromEnv(store, g)
			fmt.Println("retrieval signals:")
			fmt.Println("  fts + graph  always on")
			if r.VectorsActive() {
				fmt.Printf("  vectors      active (model %s)\n", r.VectorModel())
			} else {
				vcount, _ := store.Count("vectors")
				if vcount > 0 {
					fmt.Println("  vectors      stored but query embedder not configured (set MESH_EMBED_ENDPOINT + MESH_EMBED_MODEL)")
				} else {
					fmt.Println("  vectors      off (run: mesh embed)")
				}
			}
			if r.RerankActive() {
				fmt.Printf("  rerank       active (cross-encoder %s)\n", r.RerankModel())
			} else {
				fmt.Println("  rerank       off (set MESH_RERANK_ENDPOINT + MESH_RERANK_MODEL; see tools/rerank-server)")
			}
			if wf, wg, wv := r.Weights(); wf != 0 || wg != 0 || wv != 0 {
				fmt.Printf("  weights      learned fts=%.2f graph=%.2f vec=%.2f (MESH_WEIGHT_*)\n", wf, wg, wv)
			} else {
				fmt.Println("  weights      built-in defaults (run: mesh tune <cases.json> to fit your corpus)")
			}
			return nil
		},
	}
}

func newCmd() *cobra.Command {
	var vaultDir, do, dont, why, related, tags, status, severity, by string
	c := &cobra.Command{
		Use:   "new <type> <title...>",
		Short: "Scaffold a note with auto-filled id, timestamp, placement, and skeleton",
		Long:  "Create a note where Mesh fills everything derivable (id, when, created, placement, filename, skeleton) so the author only supplies judgment: type, title, and the do/dont/why one-liners.",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := vault.CreateNote(vaultDir, vault.NewNoteSpec{
				Type:     vault.NoteType(args[0]),
				Title:    strings.Join(args[1:], " "),
				Do:       do,
				Dont:     dont,
				Why:      why,
				Related:  splitCSV(related),
				Tags:     splitCSV(tags),
				Status:   status,
				Severity: severity,
				By:       by,
			})
			if err != nil {
				return err
			}
			fmt.Printf("created %s\n", res.Path)
			fmt.Printf("  id: %s   when: %s\n", res.ID, res.When)
			if len(res.TODOs) > 0 {
				fmt.Printf("  fill: %s\n", strings.Join(res.TODOs, "; "))
			} else {
				fmt.Println("  lint: clean")
			}
			return nil
		},
	}
	c.Flags().StringVar(&vaultDir, "vault", ".", "vault root")
	c.Flags().StringVar(&do, "do", "", "flywheel: what to do next time")
	c.Flags().StringVar(&dont, "dont", "", "flywheel: what to avoid and the failure it caused")
	c.Flags().StringVar(&why, "why", "", "flywheel: the reason or root cause")
	c.Flags().StringVar(&related, "related", "", "comma-separated [[basename]] links")
	c.Flags().StringVar(&tags, "tags", "", "comma-separated tags")
	c.Flags().StringVar(&status, "status", "", "status (decision/post-mortem)")
	c.Flags().StringVar(&severity, "severity", "", "severity (post-mortem)")
	c.Flags().StringVar(&by, "by", "", "author/contributor")
	return c
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func indexCmd() *cobra.Command {
	var dryRun bool
	var workers int
	c := &cobra.Command{
		Use:   "index [vault]",
		Short: "Parse a markdown vault into the knowledge graph",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := "."
			if len(args) == 1 {
				root = args[0]
			}
			files, err := vault.Walk(root)
			if err != nil {
				return err
			}
			w := workers
			if w <= 0 {
				w = runtime.NumCPU()
			}
			start := time.Now()
			notes, ferrs := index.ParseFiles(files, workers)
			// Store vault-relative paths: portable across machines and far
			// cheaper to carry in a token-budgeted card than an absolute path.
			for _, pn := range notes {
				if rel, err := filepath.Rel(root, pn.Path); err == nil {
					pn.Path = rel
				}
			}
			parseDur := time.Since(start)
			for _, fe := range ferrs {
				fmt.Fprintf(os.Stderr, "parse %s: %v\n", fe.Path, fe.Err)
			}
			g, issues := index.BuildGraph(notes)
			communities := g.DetectCommunities(0)
			printStats(root, len(files), len(ferrs), parseDur, w, communities, notes, g, issues)
			if dryRun {
				return nil
			}
			store, err := index.Open(root)
			if err != nil {
				return err
			}
			defer store.Close()
			n, err := store.IndexVault(notes, g)
			if err != nil {
				return err
			}
			fmt.Printf("wrote:  %d notes to %s\n", n, store.Path())
			return nil
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "parse and report without writing .mesh/mesh.db")
	c.Flags().IntVar(&workers, "workers", 0, "parse workers (0 = NumCPU)")
	return c
}

func printStats(root string, files, parseErrs int, parseDur time.Duration, workers, communities int, notes []*index.ParsedNote, g *graph.Graph, issues []index.Issue) {
	byType := map[string]int{}
	for _, n := range notes {
		byType[string(n.FM.Type)]++
	}
	byIssue := map[string]int{}
	for _, is := range issues {
		byIssue[is.Kind]++
	}

	fmt.Printf("vault:  %s\n", root)
	fmt.Printf("parse:  %d files in %s (%d workers, %d errors)\n", files, parseDur.Round(time.Microsecond), workers, parseErrs)
	fmt.Printf("nodes:  %d\n", g.NodeCount())
	for _, kv := range sortedCounts(g.CountByKind()) {
		fmt.Printf("          %-8s %d\n", kv.k, kv.v)
	}
	fmt.Printf("edges:  %d\n", g.EdgeCount())
	fmt.Printf("communs: %d\n", communities)
	fmt.Printf("types:\n")
	for _, kv := range sortedCounts(byType) {
		fmt.Printf("          %-12s %d\n", kv.k, kv.v)
	}
	if len(issues) > 0 {
		fmt.Printf("issues: %d\n", len(issues))
		for _, kv := range sortedCounts(byIssue) {
			fmt.Printf("          %-14s %d\n", kv.k, kv.v)
		}
	}
}

type kvCount struct {
	k string
	v int
}

func sortedCounts(m map[string]int) []kvCount {
	out := make([]kvCount, 0, len(m))
	for k, v := range m {
		out = append(out, kvCount{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].v != out[j].v {
			return out[i].v > out[j].v
		}
		return out[i].k < out[j].k
	})
	return out
}

func stub(use, short, milestone string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Printf("%s is planned for %s and is not implemented yet.\n", use, milestone)
			return nil
		},
	}
}

func migrateCmd() *cobra.Command {
	var dryRun bool
	c := &cobra.Command{
		Use:   "migrate [vault]",
		Short: "Bring a Hive-style vault up to the Mesh schema (idempotent)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := vaultArg(args)
			files, err := vault.Walk(root)
			if err != nil {
				return err
			}
			var changed, flywheel, errored int
			for _, f := range files {
				res, err := vault.MigrateFile(f, dryRun)
				if err != nil {
					errored++
					fmt.Fprintf(os.Stderr, "migrate %s: %v\n", f, err)
					continue
				}
				if res.Changed {
					changed++
				}
				if len(res.Issues) > 0 {
					flywheel++
				}
			}
			verb := "migrated"
			if dryRun {
				verb = "would migrate"
			}
			fmt.Printf("%s %d of %d files (%d already clean, %d errored)\n", verb, changed, len(files), len(files)-changed-errored, errored)
			if flywheel > 0 {
				fmt.Printf("note:   %d flywheel notes still need do/dont/why (author them; never auto-filled)\n", flywheel)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "preview changes without writing")
	return c
}

func lintCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "lint [vault]",
		Short: "Check vault health (frontmatter, links, ids, filenames)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := vaultArg(args)
			files, err := vault.Walk(root)
			if err != nil {
				return err
			}
			notes, ferrs := index.ParseFiles(files, 0)
			_, issues := index.BuildGraph(notes)

			byKind := map[string]int{}
			for _, is := range issues {
				byKind[is.Kind]++
			}
			for _, fe := range ferrs {
				_ = fe
				byKind["parse-error"]++
			}
			for _, pn := range notes {
				for _, e := range pn.FM.Validate() {
					if e == "missing id" {
						continue // already counted via BuildGraph issues
					}
					byKind["frontmatter"]++
				}
				if !isKebab(filepath.Base(pn.Path)) {
					byKind["filename"]++
				}
			}

			total := 0
			for _, n := range byKind {
				total += n
			}
			fmt.Printf("lint %s: %d files, %d problems\n", root, len(files), total)
			for _, kv := range sortedCounts(byKind) {
				fmt.Printf("  %-14s %d\n", kv.k, kv.v)
			}
			if total > 0 {
				return fmt.Errorf("%d lint problems", total)
			}
			fmt.Println("clean")
			return nil
		},
	}
}

func vaultArg(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	return "."
}

func isKebab(filename string) bool {
	name := strings.TrimSuffix(filename, filepath.Ext(filename))
	if name == "" {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

func mcpCmd() *cobra.Command {
	var vaultDir string
	c := &cobra.Command{
		Use:   "mcp",
		Short: "Serve the agent retrieval contract over MCP (JSON-RPC on stdio)",
		Long:  "Long-running MCP server a coding agent spawns to search, fetch, and write back to the vault. Configure your agent with: {\"command\": \"mesh\", \"args\": [\"mcp\", \"--vault\", \"<path>\"]}.",
		RunE: func(cmd *cobra.Command, args []string) error {
			srv, err := mcp.NewServer(vaultDir)
			if err != nil {
				return err
			}
			defer srv.Close()
			return srv.ServeStdio()
		},
	}
	c.Flags().StringVar(&vaultDir, "vault", ".", "vault root")
	return c
}
func tuiCmd() *cobra.Command { return stub("tui", "Open the terminal UI", "Milestone 3") }
func uiCmd() *cobra.Command  { return stub("ui", "Serve the localhost graph viewer", "Milestone 3") }
