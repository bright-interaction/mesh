package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/bright-interaction/mesh/internal/embed"
	"github.com/bright-interaction/mesh/internal/eval"
	"github.com/bright-interaction/mesh/internal/graph"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/mcp"
	"github.com/bright-interaction/mesh/internal/meshcfg"
	"github.com/bright-interaction/mesh/internal/retrieve"
	"github.com/bright-interaction/mesh/internal/sshserve"
	"github.com/bright-interaction/mesh/internal/tui"
	"github.com/bright-interaction/mesh/internal/vault"
	"github.com/bright-interaction/mesh/internal/watch"
	"github.com/bright-interaction/mesh/internal/web"
	"github.com/bright-interaction/mesh/pkg/meshclient"
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
		codeCmd(),
		embedCmd(),
		searchCmd(),
		evalCmd(),
		tuneCmd(),
		statusCmd(),
		healthCmd(),
		ingestCmd(),
		migrateCmd(),
		lintCmd(),
		structureCmd(),
		mcpCmd(),
		watchCmd(),
		joinCmd(),
		syncCmd(),
		conflictsCmd(),
		curatorCmd(),
		tuiCmd(),
		uiCmd(),
		serveSSHCmd(),
		installCmd(),
		orientCmd(),
		hooksCmd(),
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
			cards, err := retrieve.NewFromEnv(store, g).Retrieve(cmd.Context(), strings.Join(args, " "), retrieve.Options{Limit: limit, Budget: budget})
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
	var noCache bool
	c := &cobra.Command{
		Use:   "embed [vault]",
		Short: "Embed notes via a BYOAI endpoint and store vectors (turns on semantic search)",
		Long:  "Calls an OpenAI-compatible /embeddings endpoint (Ollama, OpenAI, Voyage, ...) you control. Vectors stay in .mesh/mesh.db. After this, mesh search / eval / mcp fuse the semantic signal automatically.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := vaultArg(args)
			// Resolve config flag-first, then env, then the persisted solo config.toml.
			cfg, _ := meshcfg.Load(filepath.Join(root, ".mesh"))
			if endpoint == "" {
				endpoint = firstNonEmpty(os.Getenv("MESH_EMBED_ENDPOINT"), cfg.Endpoint)
			}
			if model == "" {
				model = firstNonEmpty(os.Getenv("MESH_EMBED_MODEL"), cfg.Model)
			}
			// If --key-env was not passed, inherit the persisted key_env (so a re-embed
			// does not silently fall back to MESH_EMBED_KEY and clobber a custom name).
			if !cmd.Flags().Changed("key-env") && cfg.KeyEnv != "" {
				keyEnv = cfg.KeyEnv
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
				NodeID   string
				ChunkIx  int
				Text     string
				NoteHash string // the note's retrieval hash, stamped so retrieval can detect a later edit
			}
			var refs []chunkRef
			for _, nf := range files {
				pn, err := index.ParseFile(filepath.Join(root, nf.Path))
				if err != nil {
					return fmt.Errorf("parse %s: %w", nf.Path, err)
				}
				noteHash := index.RetrievalHash(pn)
				if !perSection {
					refs = append(refs, chunkRef{NodeID: nf.NodeID, ChunkIx: 0, Text: strings.Join(index.ChunkText(pn), "\n"), NoteHash: noteHash})
					continue
				}
				for ix, text := range index.ChunkText(pn) {
					refs = append(refs, chunkRef{NodeID: nf.NodeID, ChunkIx: ix, Text: text, NoteHash: noteHash})
				}
			}
			emb := embed.NewHTTP(endpoint, model, os.Getenv(keyEnv))
			if batch <= 0 {
				batch = 32
			}
			ctx := context.Background()
			docPrefix := firstNonEmpty(os.Getenv("MESH_EMBED_DOC_PREFIX"), cfg.DocPrefix) // e.g. "search_document: " for nomic

			// Content-hash cache: reuse the stored vector for any chunk whose embedding
			// input is unchanged since the last embed, so a re-embed only pays for the
			// changed and new chunks. The cache is model-scoped (a different model
			// invalidates it). --no-cache forces a full re-embed (e.g. if a same-named
			// model changed its output width).
			cache := map[string]index.CachedVec{}
			if !noCache {
				cache, err = store.CachedVectors(model)
				if err != nil {
					return err
				}
			}
			hashes := make([]string, len(refs))
			for i, r := range refs {
				hashes[i] = index.ContentHash(docPrefix, r.Text)
			}
			rows := make([]index.VectorRow, 0, len(refs))
			var toEmbed []int
			for idx, r := range refs {
				if c, ok := cache[index.VecKey(r.NodeID, r.ChunkIx)]; ok && c.Hash == hashes[idx] {
					rows = append(rows, index.VectorRow{NodeID: r.NodeID, ChunkIx: r.ChunkIx, Vec: c.Vec, ContentHash: hashes[idx], NoteHash: r.NoteHash})
					continue
				}
				toEmbed = append(toEmbed, idx)
			}
			reused := len(refs) - len(toEmbed)
			for i := 0; i < len(toEmbed); i += batch {
				j := min(i+batch, len(toEmbed))
				inputs := make([]string, 0, j-i)
				for _, idx := range toEmbed[i:j] {
					inputs = append(inputs, docPrefix+refs[idx].Text)
				}
				vecs, err := emb.Embed(ctx, inputs)
				if err != nil {
					return fmt.Errorf("embed batch %d-%d via %s: %w", i, j, endpoint, err)
				}
				for k, v := range vecs {
					idx := toEmbed[i+k]
					rows = append(rows, index.VectorRow{NodeID: refs[idx].NodeID, ChunkIx: refs[idx].ChunkIx, Vec: v, ContentHash: hashes[idx], NoteHash: refs[idx].NoteHash})
				}
				fmt.Printf("\rembedded %d/%d new chunks", j, len(toEmbed))
			}
			if len(toEmbed) > 0 {
				fmt.Println()
			}
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
			// Persist the solo config so mesh search / mcp work next session without
			// re-exporting env vars. Best-effort: a write failure must not fail the embed
			// (the vectors are already stored). Secrets are never written, only key_env.
			if err := meshcfg.Save(filepath.Join(root, ".mesh"), meshcfg.Embedding{
				Endpoint:    endpoint,
				Model:       model,
				Dim:         dim,
				KeyEnv:      keyEnv,
				QueryPrefix: firstNonEmpty(os.Getenv("MESH_EMBED_QUERY_PREFIX"), cfg.QueryPrefix),
				DocPrefix:   docPrefix,
			}); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not write .mesh/config.toml (%v); set MESH_EMBED_* env vars to keep semantic search on\n", err)
			}
			fmt.Printf("stored %d vectors across %d notes (%s, model %s, dim %d; %d embedded, %d reused from cache); semantic search active for mesh search / eval / mcp\n", len(rows), len(files), mode, model, dim, len(toEmbed), reused)
			return nil
		},
	}
	c.Flags().StringVar(&endpoint, "endpoint", "", "OpenAI-compatible embeddings base URL (or MESH_EMBED_ENDPOINT)")
	c.Flags().StringVar(&model, "model", "", "embedding model id (or MESH_EMBED_MODEL)")
	c.Flags().StringVar(&keyEnv, "key-env", "MESH_EMBED_KEY", "env var holding the bearer key (empty for local)")
	c.Flags().IntVar(&batch, "batch", 32, "embeddings per request")
	c.Flags().BoolVar(&perSection, "per-section", false, "store one vector per heading section instead of one per note (~18x more vectors; no measured lift on Hive)")
	c.Flags().BoolVar(&noCache, "no-cache", false, "re-embed every chunk, ignoring the content-hash cache (use if a same-named model changed its output width)")
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
			total, live, stale, _ := store.VectorStats()
			if r.VectorsActive() {
				fmt.Printf("  vectors      active (model %s, %d live", r.VectorModel(), live)
				if stale > 0 {
					fmt.Printf(", %d stale - run mesh embed to refresh", stale)
				}
				if r.HNSWActive() {
					fmt.Print(", ANN/hnsw")
				}
				fmt.Println(")")
			} else if total > 0 {
				fmt.Println("  vectors      stored but query embedder not configured (re-run mesh embed, or set MESH_EMBED_ENDPOINT + MESH_EMBED_MODEL)")
			} else {
				fmt.Println("  vectors      off (run: mesh embed)")
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

func ingestCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ingest",
		Short: "Pull external knowledge (GitHub, Slack, Linear, Jira, Notion) into the vault",
		Long:  "Sovereign ingestion: import from where your team already keeps knowledge, into YOUR vault on YOUR hardware. Each item becomes a note under imported/<source>/ with source/source_url/imported_at provenance, upserted (a re-pull updates, never duplicates). Pulls are incremental (a high-water mark per source in .mesh/ingest-state.json); --full re-pulls everything, --watch <dur> keeps pulling on a schedule, and `ingest all` runs every source listed in .mesh/ingest.json. Tokens come from env (never flags/config): MESH_INGEST_{GITHUB,SLACK,LINEAR,JIRA,NOTION}_TOKEN.",
	}
	c.AddCommand(ingestGitHubCmd(), ingestSlackCmd(), ingestLinearCmd(), ingestJiraCmd(), ingestNotionCmd(), ingestAllCmd())
	return c
}

func healthCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "health [vault]",
		Short: "Check knowledge lifecycle: dead source refs, overdue reviews, contradictions",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := "."
			if len(args) == 1 {
				root = args[0]
			}
			if _, err := os.Stat(filepath.Join(root, ".mesh", "mesh.db")); err != nil {
				return fmt.Errorf("no index (run: mesh index %s)", root)
			}
			store, err := index.Open(root)
			if err != nil {
				return err
			}
			defer store.Close()
			now := time.Now()
			if _, err := store.ComputeHealth(root, now); err != nil {
				return err
			}
			if _, err := store.ComputeContradictions(now); err != nil {
				return err
			}
			counts, _ := store.HealthCounts()
			findings, err := store.ListHealth("")
			if err != nil {
				return err
			}
			if len(findings) == 0 {
				fmt.Println("vault healthy: no dead refs, overdue reviews, or contradictions")
				return nil
			}
			fmt.Printf("health: %d dead refs, %d overdue, %d contradictions\n\n",
				counts["dead_ref"], counts["overdue"], counts["contradiction"])
			for _, f := range findings {
				fmt.Printf("  [%s] %s - %s\n", f.Issue, f.Path, f.Detail)
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

// codeCmd is the source-code index: the pure-Go graphify replacement. It walks the
// configured code roots (separate from the note vault), extracts symbols (Go via the
// stdlib AST with a call graph; other languages via a declaration scanner), and lets
// mesh_code_search / mesh_code_neighbors locate definitions by name.
func codeCmd() *cobra.Command {
	c := &cobra.Command{Use: "code", Short: "Source-code index (the graphify replacement): locate symbols + Go call graph"}
	c.AddCommand(codeReindexCmd(), codeSearchCmd())
	return c
}

func codeReindexCmd() *cobra.Command {
	var rootsFlag []string
	var langsFlag string
	c := &cobra.Command{
		Use:   "reindex [vault]",
		Short: "Walk the configured code roots and (re)build the source-code index",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := "."
			if len(args) == 1 {
				root = args[0]
			}
			store, err := index.Open(root)
			if err != nil {
				return err
			}
			defer store.Close()
			cfg, _ := meshcfg.LoadConfig(store.MeshDir())
			roots := rootsFlag
			if len(roots) == 0 {
				roots = cfg.Code.Roots
			}
			if env := os.Getenv("MESH_CODE_ROOTS"); env != "" && len(rootsFlag) == 0 {
				roots = strings.Split(env, ",")
			}
			if len(roots) == 0 {
				return fmt.Errorf("no code roots: set [code] roots in %s or pass --root", filepath.Join(store.MeshDir(), "config.toml"))
			}
			langs := cfg.Code.Languages
			if langsFlag != "" {
				langs = strings.Split(langsFlag, ",")
			}
			start := time.Now()
			st, err := index.ReindexCode(store, roots, codeLangSet(langs))
			if err != nil {
				return err
			}
			fmt.Printf("code index: %d files, %d symbols, %d edges in %s\n  roots: %s\n  db:    %s\n",
				st.Files, st.Symbols, st.Edges, time.Since(start).Round(time.Millisecond), strings.Join(roots, ", "), store.Path())
			return nil
		},
	}
	c.Flags().StringSliceVar(&rootsFlag, "root", nil, "code root to index (repeatable); overrides config")
	c.Flags().StringVar(&langsFlag, "languages", "", "comma list of language tags (default: config or all)")
	return c
}

func codeSearchCmd() *cobra.Command {
	var vaultRoot, langs string
	var limit int
	c := &cobra.Command{
		Use:   "search <query>",
		Short: "Search the source-code symbol index (file:line results)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := index.Open(vaultRoot)
			if err != nil {
				return err
			}
			defer store.Close()
			var langList []string
			if langs != "" {
				langList = strings.Split(langs, ",")
			}
			hits, err := store.SearchCode(strings.Join(args, " "), limit, langList)
			if err != nil {
				return err
			}
			for _, h := range hits {
				fmt.Printf("%-9s %-40s %s:%d\n", h.Kind, h.Name, h.Path, h.Line)
			}
			fmt.Printf("(%d symbols)\n", len(hits))
			return nil
		},
	}
	c.Flags().StringVar(&vaultRoot, "vault", ".", "vault root (the .mesh/mesh.db location)")
	c.Flags().IntVar(&limit, "limit", 15, "max results")
	c.Flags().StringVar(&langs, "languages", "", "comma list of language tags to filter")
	return c
}

func codeLangSet(langs []string) map[string]bool {
	if len(langs) == 0 {
		return nil
	}
	m := map[string]bool{}
	for _, l := range langs {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" {
			m[l] = true
		}
	}
	return m
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

// firstNonEmpty returns the first non-empty string (the config-resolution chain:
// flag, then env, then persisted config.toml).
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
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
	var doWatch bool
	var httpAddr, httpToken string
	var debounce, reconcile time.Duration
	c := &cobra.Command{
		Use:   "mcp",
		Short: "Serve the agent retrieval contract over MCP (JSON-RPC on stdio, or HTTP with --http)",
		Long:  "Long-running MCP server a coding agent spawns to search, fetch, and write back to the vault. Default transport is stdio: {\"command\": \"mesh\", \"args\": [\"mcp\", \"--vault\", \"<path>\"]}. Use --http :PORT to serve over HTTP instead (POST /mcp) so any remote MCP client (Claude, Cursor, ChatGPT, ...) connects without a local install; a bearer --token is REQUIRED when binding beyond loopback. Add --watch so editor changes are searchable in the same session.",
		RunE: func(cmd *cobra.Command, args []string) error {
			srv, err := mcp.NewServer(vaultDir)
			if err != nil {
				return err
			}
			defer srv.Close()
			if httpAddr != "" {
				return serveMCPHTTP(srv, httpAddr, httpToken, doWatch, debounce, reconcile)
			}
			if !doWatch {
				return srv.ServeStdio()
			}
			// Background watcher keeps the in-memory index fresh while the stdio
			// loop serves the agent. On stdin EOF (agent disconnect) ServeStdio
			// returns; we then stop the watcher and wait for it before Close.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				logf := func(format string, a ...any) {
					fmt.Fprintf(os.Stderr, "mesh watch: "+format+"\n", a...)
				}
				if err := srv.Watch(ctx, debounce, reconcile, logf); err != nil {
					fmt.Fprintf(os.Stderr, "mesh watch: %v\n", err)
				}
			}()
			serveErr := srv.ServeStdio()
			cancel()
			<-done
			return serveErr
		},
	}
	c.Flags().StringVar(&vaultDir, "vault", ".", "vault root")
	c.Flags().BoolVar(&doWatch, "watch", false, "live-reindex the vault in the background so editor changes are searchable without a restart")
	c.Flags().StringVar(&httpAddr, "http", "", "serve MCP over HTTP at host:port (POST /mcp) instead of stdio")
	c.Flags().StringVar(&httpToken, "token", "", "bearer token for --http (or MESH_MCP_TOKEN); REQUIRED when binding beyond loopback")
	c.Flags().DurationVar(&debounce, "debounce", 300*time.Millisecond, "quiet window to coalesce a burst of saves")
	c.Flags().DurationVar(&reconcile, "reconcile", 30*time.Second, "periodic full-reconcile safety net (0 to disable)")
	return c
}

// serveMCPHTTP serves the MCP server over HTTP (POST /mcp). Fail-closed: a
// non-loopback bind REQUIRES a bearer token. Optionally runs the background watcher.
func serveMCPHTTP(srv *mcp.Server, addr, token string, doWatch bool, debounce, reconcile time.Duration) error {
	if token == "" {
		token = os.Getenv("MESH_MCP_TOKEN")
	}
	if !addrIsLoopback(addr) && token == "" {
		return fmt.Errorf("refusing to bind %s without a token: set --token or MESH_MCP_TOKEN (fail-closed)", addr)
	}
	if doWatch {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, "mesh watch: "+format+"\n", a...) }
			_ = srv.Watch(ctx, debounce, reconcile, logf)
		}()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp", func(w http.ResponseWriter, r *http.Request) {
		if token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		srv.HandleHTTP(w, r)
	})
	httpSrv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	fmt.Fprintf(os.Stderr, "mesh mcp: serving HTTP at %s/mcp (auth: %v)\n", addr, token != "")
	return httpSrv.ListenAndServe()
}

// addrIsLoopback reports whether host:port binds only the loopback interface.
func addrIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return false // ":7575" binds all interfaces
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func watchCmd() *cobra.Command {
	var debounce, reconcile time.Duration
	c := &cobra.Command{
		Use:   "watch [vault]",
		Short: "Watch the vault and live-reindex on every change (local-first immediacy)",
		Long:  "Long-running reindexer: edit a note in your editor and it is searchable at once, no commit, no manual mesh index. A reconcile runs at startup, on every change (debounced), and on a periodic safety tick that always converges. Keeps .mesh/mesh.db fresh for mesh search and any reader; for a live MCP session, run mesh mcp --watch instead so the server hot-reloads its own in-memory index.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := vaultArg(args)
			store, err := index.Open(root)
			if err != nil {
				return err
			}
			defer store.Close()

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			abs, _ := filepath.Abs(root)
			n, _ := store.Count("notes")
			fmt.Printf("watching %s (%d notes indexed); edits reindex live. Ctrl-C to stop.\n", abs, n)
			logf := func(format string, a ...any) {
				fmt.Printf("%s  "+format+"\n", append([]any{time.Now().Format("15:04:05")}, a...)...)
			}
			// Incremental: the first reconcile seeds the cache (full), later ones parse
			// only changed files and rebuild the graph in memory.
			live := index.NewLiveIndexer(store, root)
			err = watch.Run(ctx, watch.Options{
				Root:      root,
				Debounce:  debounce,
				Reconcile: reconcile,
				Logf:      logf,
				OnReindex: func(authoritative bool) (watch.Result, error) {
					rec, err := live.Reconcile(authoritative)
					if err != nil {
						return watch.Result{}, err
					}
					return watch.Result{
						Added:     rec.Added,
						Changed:   rec.Changed,
						Removed:   rec.Removed,
						Reindexed: rec.Reindexed,
						Dur:       rec.Dur,
					}, nil
				},
			})
			fmt.Println("stopped.")
			return err
		},
	}
	c.Flags().DurationVar(&debounce, "debounce", 300*time.Millisecond, "quiet window to coalesce a burst of saves")
	c.Flags().DurationVar(&reconcile, "reconcile", 30*time.Second, "periodic full-reconcile safety net (0 to disable)")
	return c
}
func joinCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "join <hub-url> <invite-token> [vault]",
		Short: "Join a team vault: redeem an invite and clone it, no git needed",
		Long:  "Redeem a one-time invite from a mesh-hub, store the client token under <vault>/.mesh, fail closed if the team embedding config conflicts with yours, then clone the vault via a reconcile. After this, edit locally and run mesh sync.",
		Args:  cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			hubURL, invite := args[0], args[1]
			vaultDir := "."
			if len(args) == 3 {
				vaultDir = args[2]
			}
			sum, err := meshclient.JoinVault(hubURL, invite, vaultDir)
			if err != nil {
				return err
			}
			store, err := index.Open(vaultDir)
			if err != nil {
				return err
			}
			defer store.Close()
			if _, err := index.Reconcile(store, vaultDir); err != nil {
				return err
			}
			abs, _ := filepath.Abs(vaultDir)
			fmt.Printf("joined and cloned %s (HEAD %s, %d files pulled)\n", abs, short8(sum.Head), sum.Pulled)
			fmt.Println("next:")
			fmt.Println("  mesh sync " + vaultDir + "                       # push your edits, pull teammates'")
			fmt.Printf("  mesh mcp --vault %s --watch       # point your agent at the vault\n", vaultDir)
			return nil
		},
	}
	return c
}

func syncCmd() *cobra.Command {
	var doWatch bool
	var debounce, reconcile time.Duration
	c := &cobra.Command{
		Use:   "sync [vault]",
		Short: "Reconcile the vault with the hub (push local edits, pull teammates', no git)",
		Long:  "One pull-based reconcile round: pushes your changed notes, applies the hub's merged result and any teammates' changes, and reindexes. Additive edits to a shared page auto-merge; a true overwrite keeps the hub version and saves yours to a *.sync-conflict sibling. Add --watch to stay running: local edits push and the hub's changes pull in real time (SSE), with a periodic reconcile as the safety net.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			vaultDir := vaultArg(args)
			store, err := index.Open(vaultDir)
			if err != nil {
				return err
			}
			defer store.Close()

			// One sync round: push/pull via the hub, then reindex so search reflects
			// the merged result. Reused by the one-shot path and the watch loop. The
			// LiveIndexer makes the watch loop incremental (full seed on the first call,
			// targeted updates after); the one-shot path just does that single full pass.
			live := index.NewLiveIndexer(store, vaultDir)
			syncOnce := func(authoritative bool) (meshclient.Summary, error) {
				sum, err := meshclient.SyncVault(vaultDir)
				if err != nil {
					return sum, err
				}
				if _, err := live.Reconcile(authoritative); err != nil {
					return sum, err
				}
				return sum, nil
			}

			if !doWatch {
				sum, err := syncOnce(true) // one-shot: full authoritative reconcile
				if err != nil {
					return err
				}
				fmt.Printf("synced: pushed %d, pulled %d, %d conflict(s) (HEAD %s)\n", sum.Pushed, sum.Pulled, sum.Conflicts, short8(sum.Head))
				for _, sib := range sum.ConflictSiblings {
					fmt.Printf("  conflict: hub version kept; your version saved at %s (resolve, then sync)\n", sib)
				}
				for _, sib := range sum.Protected {
					fmt.Printf("  protected your unsaved local edit; incoming hub version saved at %s\n", sib)
				}
				return nil
			}

			// --watch: continuous reconcile driven by three sources, all funneled
			// through the watcher's single-flight loop: local .md edits (fsnotify),
			// the hub's SSE "head changed" nudges (Trigger), and a periodic safety
			// tick. The startup reconcile does the initial sync.
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			abs, _ := filepath.Abs(vaultDir)
			fmt.Printf("syncing %s continuously: local edits push, hub changes pull. Ctrl-C to stop.\n", abs)
			logf := func(format string, a ...any) {
				fmt.Printf("%s  "+format+"\n", append([]any{time.Now().Format("15:04:05")}, a...)...)
			}
			meshclient.Logf = logf // surface non-fatal stream diagnostics (e.g. auth rejections)
			nudge := make(chan struct{}, 1)
			go func() {
				if err := meshclient.StreamEvents(ctx, vaultDir, nudge); err != nil {
					logf("event stream unavailable, falling back to periodic sync: %v", err)
				}
			}()
			err = watch.Run(ctx, watch.Options{
				Root:      vaultDir,
				Debounce:  debounce,
				Reconcile: reconcile,
				Trigger:   nudge,
				Logf:      logf,
				OnReindex: func(authoritative bool) (watch.Result, error) {
					// Note: applying the hub's deltas writes .md files, which the
					// watcher sees and debounces into one more reconcile. That follow-up
					// is a guaranteed no-op (SyncVault re-hashes from disk and persists
					// the new base, so the next computeOutbox is empty and the hub
					// fast-forwards without a broadcast), so it converges in one extra
					// idle round rather than looping. We accept that cheap round instead
					// of threading self-written paths through the watcher.
					sum, serr := syncOnce(authoritative)
					if serr != nil {
						return watch.Result{}, serr
					}
					// Log a sync-centric line ourselves only when something moved, then
					// return Reindexed:false so the watcher does not also log its
					// generic reindex line.
					if sum.Pushed > 0 || sum.Pulled > 0 || sum.Conflicts > 0 || len(sum.Protected) > 0 {
						logf("synced: pushed %d, pulled %d, %d conflict(s) (HEAD %s)", sum.Pushed, sum.Pulled, sum.Conflicts, short8(sum.Head))
						for _, sib := range sum.ConflictSiblings {
							logf("  conflict: hub version kept; your version saved at %s", sib)
						}
						for _, sib := range sum.Protected {
							logf("  protected your local edit; incoming hub version saved at %s", sib)
						}
					}
					return watch.Result{Reindexed: false}, nil
				},
			})
			fmt.Println("stopped.")
			return err
		},
	}
	c.Flags().BoolVar(&doWatch, "watch", false, "stay running: push local edits and pull hub changes in real time (SSE) plus a periodic safety reconcile")
	c.Flags().DurationVar(&debounce, "debounce", 500*time.Millisecond, "quiet window to coalesce a burst of local saves before syncing")
	c.Flags().DurationVar(&reconcile, "reconcile", 60*time.Second, "periodic safety-net sync interval (0 to disable)")
	return c
}

func short8(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	if s == "" {
		return "(none)"
	}
	return s
}

func serveSSHCmd() *cobra.Command {
	var addr, hostKey, authKeys string
	var allowAnon bool
	c := &cobra.Command{
		Use:   "serve-ssh [vault]",
		Short: "Serve the TUI over SSH (ssh into your knowledge graph, no local install)",
		Long: "Run an SSH server that hands every connection the Mesh TUI over the same index the agent uses, " +
			"so a teammate browses the graph with `ssh -p <port> <host>` and no Mesh install. Read-only. " +
			"Auth is fail-closed: pass --authorized-keys (OpenSSH format) and only those keys may connect; " +
			"--allow-anonymous opts out for a localhost demo.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			vaultDir := vaultArg(args)
			if hostKey == "" {
				hostKey = filepath.Join(vaultDir, ".mesh", "ssh_host_ed25519_key")
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			logf := func(format string, a ...any) {
				fmt.Printf("%s  "+format+"\n", append([]any{time.Now().Format("15:04:05")}, a...)...)
			}
			err := sshserve.Serve(ctx, vaultDir, sshserve.Options{
				Addr: addr, HostKeyPath: hostKey, AuthKeysPath: authKeys, AllowAnon: allowAnon, Logf: logf,
			})
			fmt.Println("stopped.")
			return err
		},
	}
	c.Flags().StringVar(&addr, "addr", ":2222", "listen address")
	c.Flags().StringVar(&hostKey, "host-key", "", "host key path (default <vault>/.mesh/ssh_host_ed25519_key; generated if missing)")
	c.Flags().StringVar(&authKeys, "authorized-keys", "", "OpenSSH authorized_keys file; only these public keys may connect (required unless --allow-anonymous)")
	c.Flags().BoolVar(&allowAnon, "allow-anonymous", false, "DANGER: serve with no auth (localhost demos only)")
	return c
}

func tuiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tui [vault]",
		Short: "Open the keyboard-driven terminal view of the vault",
		Long:  "A three-pane terminal UI over the same index + graph the agent uses: browse the notes list (hub-first), search (the same ranked cards as the agent), and preview a note with its frontmatter and neighbors. Keyboard-driven; press ? for help.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return tui.Run(vaultArg(args))
		},
	}
}
func uiCmd() *cobra.Command {
	var addr, token, basePath string
	c := &cobra.Command{
		Use:   "ui [vault]",
		Short: "Serve the local web app: graph, search, settings, docs, API reference",
		Long:  "Open the Mesh web app for a vault: the graph (force + galaxy + 3D), a search view, editable settings, in-app docs, and the API reference. Same index the agent reads over MCP, served from the single binary with no CDN. Loopback bind needs no auth; binding beyond loopback requires --token (or MESH_UI_TOKEN), fail-closed. Use --base-path to serve under a path (e.g. behind a reverse proxy at /app).",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if token == "" {
				token = os.Getenv("MESH_UI_TOKEN")
			}
			if basePath == "" {
				basePath = os.Getenv("MESH_UI_BASE_PATH")
			}
			return web.Serve(vaultArg(args), addr, token, basePath)
		},
	}
	c.Flags().StringVar(&addr, "addr", "127.0.0.1:7474", "host:port to bind the local viewer")
	c.Flags().StringVar(&token, "token", "", "bearer token required for /api access (or MESH_UI_TOKEN); mandatory when binding beyond loopback")
	c.Flags().StringVar(&basePath, "base-path", "", "serve the app under a path, e.g. /app (or MESH_UI_BASE_PATH), for a reverse proxy")
	return c
}
