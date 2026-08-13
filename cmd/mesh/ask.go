// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"fmt"
	"strings"

	"github.com/bright-interaction/mesh/internal/ask"
	"github.com/bright-interaction/mesh/internal/index"
	"github.com/bright-interaction/mesh/internal/llm"
	"github.com/bright-interaction/mesh/internal/retrieve"
	"github.com/bright-interaction/mesh/internal/shellpath"
	"github.com/spf13/cobra"
)

// askCmd answers a natural-language question from the vault (notes + code) via the
// BYOAI LLM, grounded with citations. The conversational second brain.
func askCmd() *cobra.Command {
	var vaultRoot string
	var budget int
	c := &cobra.Command{
		Use:   "ask <question>",
		Short: "Answer a question from your notes + code (BYOAI, grounded with citations)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Read-only: ask retrieves and answers, it never writes the index (the
			// retriever reads vectors and FTS, ask.Answer reads code hits). A writable
			// open would take the SQLite write lock away from the owning writer for a
			// pure read, and would silently CREATE an empty index on a vault that has
			// none, so the LLM would answer "nothing in your notes about that" from an
			// empty database instead of the command saying there is no index yet.
			store, err := index.OpenReadOnly(vaultRoot)
			if err != nil {
				return fmt.Errorf("mesh ask needs a built index (run: mesh index %s): %w", shellpath.Quote(vaultRoot), err)
			}
			defer store.Close()
			client, err := llm.NewFromEnv()
			if err != nil {
				return err
			}
			g, err := store.LoadGraph()
			if err != nil {
				return err
			}
			rtr := retrieve.NewFromEnv(store, g)
			res, err := ask.Answer(cmd.Context(), rtr, store, client, strings.Join(args, " "), budget, nil, nil)
			if err != nil {
				return err
			}
			fmt.Println(res.Answer)
			if len(res.Citations) > 0 {
				fmt.Println("\nsources:")
				for _, c := range res.Citations {
					fmt.Printf("  [%d] %s %s (%s)\n", c.N, c.Kind, c.Title, c.Loc)
				}
			}
			return nil
		},
	}
	c.Flags().StringVar(&vaultRoot, "vault", ".", "vault root")
	c.Flags().IntVar(&budget, "budget", 3000, "token budget for the grounding context (note bodies + code); lower-ranked sources are dropped, not cited")
	return c
}
