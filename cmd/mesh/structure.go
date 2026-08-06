// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/relate"
	"github.com/bright-interaction/mesh/internal/retrieve"
	"github.com/bright-interaction/mesh/internal/vault"
	"github.com/spf13/cobra"
)

// structureCmd grades how well a vault is ORGANIZED (canonical types, note-to-note
// connectivity, tier-0 capture, a map per cluster) against the standard in
// the vault structure standard. It complements `mesh lint` (frontmatter validity) and
// `mesh health` (knowledge lifecycle): validity, organization, lifecycle.
func structureCmd() *cobra.Command {
	var verbose, wireOrphans, apply, fillBodies bool
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
			parsed, parseErrs := index.ParseFiles(files, 0)
			for _, pn := range parsed {
				if rel, err := filepath.Rel(root, pn.Path); err == nil {
					pn.Path = rel
				}
			}
			for i := range parseErrs {
				if rel, err := filepath.Rel(root, parseErrs[i].Path); err == nil {
					parseErrs[i].Path = rel
				}
			}
			g, _ := index.BuildGraph(parsed)
			g.DetectCommunities(0)
			rep := index.AnalyzeStructure(g, parsed, parseErrs)

			if wireOrphans {
				return wireOrphanNotes(cmd, root, rep, apply)
			}
			if fillBodies {
				return fillNoteBodies(root, files, apply)
			}

			fmt.Printf("structure: grade %s  (%d/100)\n", rep.Grade, rep.Score)
			fmt.Printf("  %d notes, %d clusters, %d tier-0 (decisions/gotchas/post-mortems)\n", rep.Notes, rep.Clusters, rep.Tier0)
			if rep.Unparseable > 0 {
				fmt.Printf("  !! %d note(s) fail to parse and are INVISIBLE to search/the graph - fix these first (see --verbose)\n", rep.Unparseable)
			}

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
	c.Flags().BoolVar(&wireOrphans, "wire-orphans", false, "propose `related:` links for every orphan note (dry run unless --apply)")
	c.Flags().BoolVar(&fillBodies, "fill-bodies", false, "fill a note's TODO-skeleton body sections from its already-authored do/dont/why (dry run unless --apply)")
	c.Flags().BoolVar(&apply, "apply", false, "with --wire-orphans or --fill-bodies, write the changes into the notes")
	return c
}

// fillNoteBodies repairs every note whose body is still the TODO skeleton scaffolded by an
// older Mesh while its do/dont/why already sit in frontmatter (see vault.BackfillBodyFile).
// It is a dry run unless --apply, matching wireOrphanNotes: this rewrites the author's
// files, so the default has to be "show me".
//
// BackfillBodyFile is idempotent (a filled section no longer matches its placeholder), so
// running this twice is safe and the second run reports nothing to do.
func fillNoteBodies(root string, files []string, apply bool) error {
	if !apply {
		fmt.Println("dry run (pass --apply to write); scanning for TODO-skeleton bodies with filled frontmatter")
	}
	var changed, skipped int
	for _, path := range files {
		rel := path
		if r, err := filepath.Rel(root, path); err == nil {
			rel = r
		}
		res, err := vault.BackfillBodyFile(path, !apply)
		if err != nil {
			fmt.Printf("  !! %s: %v\n", rel, err)
			skipped++
			continue
		}
		if !res.Changed {
			continue
		}
		changed++
		fmt.Printf("  %s\n", rel)
		for _, a := range res.Actions {
			fmt.Printf("      %s\n", a)
		}
	}
	verb := "would fill"
	if apply {
		verb = "filled"
	}
	fmt.Printf("\n%s %d note body(ies)", verb, changed)
	if skipped > 0 {
		fmt.Printf(", skipped %d", skipped)
	}
	fmt.Println()
	if apply && changed > 0 {
		fmt.Printf("run `mesh index %s` to pick the new bodies up\n", root)
	}
	return nil
}

// wireOrphanNotes gives every orphan note a `related:` list derived from Mesh's own
// retrieval, so notes stop arriving disconnected and staying that way. It is a dry run
// unless --apply: this rewrites the author's files, so the default has to be "show me".
//
// Notes that already declare `related` are skipped by BackfillRelatedFile, so running
// this twice is safe and the second run reports nothing to do.
func wireOrphanNotes(cmd *cobra.Command, root string, rep index.StructureReport, apply bool) error {
	var orphans []index.StructureFinding
	for _, f := range rep.Findings {
		if f.Kind == "orphan" {
			orphans = append(orphans, f)
		}
	}
	if len(orphans) == 0 {
		fmt.Println("no orphans: every note links to at least one other note")
		return nil
	}

	store, err := index.Open(root)
	if err != nil {
		return fmt.Errorf("wire-orphans needs a built index (run: mesh index %s): %w", root, err)
	}
	defer store.Close()
	ig, err := store.LoadGraph()
	if err != nil {
		return err
	}
	rt := retrieve.NewFromEnv(store, ig)

	if !apply {
		fmt.Printf("%d orphan(s); proposing up to 3 links each (dry run, pass --apply to write)\n\n", len(orphans))
	}
	var changed, skipped int
	for _, f := range orphans {
		path := filepath.Join(root, f.Path)
		id := strings.TrimSuffix(filepath.Base(f.Path), filepath.Ext(f.Path))
		// The slug IS the title, hyphenated: Slugify built it from the title when the
		// note was scaffolded, so it is the closest thing to the note's own words that
		// is available here without re-reading the file.
		query := strings.ReplaceAll(id, "-", " ")
		links := relate.Derive(cmd.Context(), rt, ig, query, id, relate.TagsOf(ig, id), 3)
		if len(links) == 0 {
			skipped++
			continue
		}
		res, err := vault.BackfillRelatedFile(path, links, !apply)
		if err != nil {
			fmt.Printf("  !! %s: %v\n", f.Path, err)
			skipped++
			continue
		}
		if !res.Changed {
			skipped++
			continue
		}
		changed++
		fmt.Printf("  %s\n", f.Path)
		for _, l := range links {
			fmt.Printf("      -> %s\n", l)
		}
	}
	verb := "would wire"
	if apply {
		verb = "wired"
	}
	fmt.Printf("\n%s %d note(s), skipped %d\n", verb, changed, skipped)
	if apply && changed > 0 {
		fmt.Printf("run `mesh index %s` to pick the new links up\n", root)
	}
	return nil
}
