package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/brightinteraction/mesh/internal/eval"
	"github.com/brightinteraction/mesh/internal/graph"
	"github.com/brightinteraction/mesh/internal/index"
	"github.com/brightinteraction/mesh/internal/mcp"
	"github.com/brightinteraction/mesh/internal/retrieve"
	"github.com/brightinteraction/mesh/internal/vault"
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
		newCmd(),
		indexCmd(),
		searchCmd(),
		evalCmd(),
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
			cards, err := retrieve.New(store, g).Retrieve(strings.Join(args, " "), retrieve.Options{Limit: limit, Budget: budget})
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
			rep := eval.RunGate(store, retrieve.New(store, g), vaultDir, cases, budget)

			fmt.Printf("Gate 1: Mesh vs read-top-%d-FTS  (vault: %s, %d cases, budget %d, tokenizer: estimate)\n", 3, vaultDir, rep.N, budget)
			for _, cr := range rep.Cases {
				fmt.Printf("  %-34s mesh[hit=%-5v %4dt]  base[hit=%-5v %4dt]\n", truncate(cr.Query, 34), cr.MeshHit, cr.MeshTokens, cr.BaseHit, cr.BaseTokens)
			}
			fmt.Printf("  mesh:     recall %d/%d   avg %.0f tok\n", rep.MeshHits, rep.N, rep.MeshAvg)
			fmt.Printf("  baseline: recall %d/%d   avg %.0f tok\n", rep.BaseHits, rep.N, rep.BaseAvg)
			if rep.BaseAvg > 0 {
				fmt.Printf("  token saving: %.0f%%\n", 100*(1-rep.MeshAvg/rep.BaseAvg))
			}
			if rep.Pass {
				fmt.Println("  VERDICT: PASS (>= baseline recall at fewer tokens)")
				return nil
			}
			fmt.Println("  VERDICT: FAIL")
			return fmt.Errorf("gate 1 not met")
		},
	}
	c.Flags().StringVar(&vaultDir, "vault", ".", "vault root")
	c.Flags().IntVar(&budget, "budget", 0, "token budget for the Mesh arm (0 = unbudgeted)")
	return c
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
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
			} {
				n, err := store.Count(t.table)
				if err != nil {
					return err
				}
				fmt.Printf("  %-9s %d\n", t.label, n)
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
func tuiCmd() *cobra.Command     { return stub("tui", "Open the terminal UI", "Milestone 3") }
func uiCmd() *cobra.Command      { return stub("ui", "Serve the localhost graph viewer", "Milestone 3") }
func doctorCmd() *cobra.Command  { return stub("doctor", "Diagnose index drift and config", "Milestone 0") }
