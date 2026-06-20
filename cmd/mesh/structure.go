package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/vault"
	"github.com/spf13/cobra"
)

// structureCmd grades how well a vault is ORGANIZED (canonical types, note-to-note
// connectivity, tier-0 capture, a map per cluster) against the standard in
// ORGANIZATION.md. It complements `mesh lint` (frontmatter validity) and
// `mesh health` (knowledge lifecycle): validity, organization, lifecycle.
func structureCmd() *cobra.Command {
	var verbose bool
	c := &cobra.Command{
		Use:   "structure [vault]",
		Short: "Grade the vault's organization: types, connectivity, tier-0, maps",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := vaultArg(args)
			files, err := vault.Walk(root)
			if err != nil {
				return err
			}
			parsed, _ := index.ParseFiles(files, 0)
			for _, pn := range parsed {
				if rel, err := filepath.Rel(root, pn.Path); err == nil {
					pn.Path = rel
				}
			}
			g, _ := index.BuildGraph(parsed)
			g.DetectCommunities(0)
			rep := index.AnalyzeStructure(g, parsed)

			fmt.Printf("structure: grade %s  (%d/100)\n", rep.Grade, rep.Score)
			fmt.Printf("  %d notes, %d clusters, %d tier-0 (decisions/gotchas/post-mortems)\n", rep.Notes, rep.Clusters, rep.Tier0)

			fmt.Print("  types:  ")
			for _, kv := range sortedCounts(rep.ByType) {
				label := kv.k
				if label == "" {
					label = "(none)"
				}
				fmt.Printf("%s %d  ", label, kv.v)
			}
			fmt.Println()

			if len(rep.Findings) == 0 {
				fmt.Println("status: well-organized")
				return nil
			}
			counts := map[string]int{}
			for _, f := range rep.Findings {
				counts[f.Kind]++
			}
			fmt.Print("  fix:    ")
			for _, kv := range sortedCounts(counts) {
				fmt.Printf("%s %d  ", kv.k, kv.v)
			}
			fmt.Println()

			if verbose {
				for _, f := range rep.Findings {
					where := f.Path
					if where == "" {
						where = "(cluster)"
					}
					fmt.Printf("    [%s] %-16s %s\n              %s\n", f.Severity, f.Kind, where, f.Detail)
				}
				for _, ci := range rep.MaplessClusters {
					fmt.Printf("\n  cluster #%d needs a map - %d notes, most-connected first:\n", ci.ID, ci.Size)
					for i, m := range ci.Members {
						if i >= 16 {
							fmt.Printf("    ... and %d more\n", ci.Size-16)
							break
						}
						key := filepath.Base(m.Path)
						key = strings.TrimSuffix(key, filepath.Ext(key))
						fmt.Printf("    %-9s [[%s]]  %s\n", m.Type, key, m.Title)
					}
				}
			} else {
				fmt.Println("  run `mesh structure --verbose` for the per-note list")
			}
			return nil
		},
	}
	c.Flags().BoolVar(&verbose, "verbose", false, "list every finding with its note path")
	return c
}
