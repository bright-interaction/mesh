package main

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/brightinteraction/mesh/internal/graph"
	"github.com/brightinteraction/mesh/internal/index"
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
		migrateCmd(),
		lintCmd(),
		mcpCmd(),
		tuiCmd(),
		uiCmd(),
		doctorCmd(),
	)
	return root
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
			parseDur := time.Since(start)
			for _, fe := range ferrs {
				fmt.Fprintf(os.Stderr, "parse %s: %v\n", fe.Path, fe.Err)
			}
			g, issues := index.BuildGraph(notes)
			printStats(root, len(files), len(ferrs), parseDur, w, notes, g, issues)
			if !dryRun {
				return fmt.Errorf("writing .mesh/mesh.db is not implemented yet (next M0 step); run with --dry-run")
			}
			return nil
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", true, "parse and report without writing .mesh/mesh.db")
	c.Flags().IntVar(&workers, "workers", 0, "parse workers (0 = NumCPU)")
	return c
}

func printStats(root string, files, parseErrs int, parseDur time.Duration, workers int, notes []*index.ParsedNote, g *graph.Graph, issues []index.Issue) {
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

func migrateCmd() *cobra.Command { return stub("migrate [vault]", "Migrate a Hive-style vault to the Mesh schema", "Milestone 0 (next step)") }
func lintCmd() *cobra.Command    { return stub("lint [vault]", "Check vault health (links, frontmatter, orphans)", "Milestone 0") }
func mcpCmd() *cobra.Command     { return stub("mcp", "Serve the agent retrieval contract over MCP", "Milestone 1") }
func tuiCmd() *cobra.Command     { return stub("tui", "Open the terminal UI", "Milestone 3") }
func uiCmd() *cobra.Command      { return stub("ui", "Serve the localhost graph viewer", "Milestone 3") }
func doctorCmd() *cobra.Command  { return stub("doctor", "Diagnose index drift and config", "Milestone 0") }
