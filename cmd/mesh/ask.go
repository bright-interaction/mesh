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
			store, err := index.Open(vaultRoot)
			if err != nil {
				return err
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
			res, err := ask.Answer(cmd.Context(), rtr, store, client, strings.Join(args, " "), budget, nil)
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
	c.Flags().IntVar(&budget, "budget", 3000, "retrieval context token budget")
	return c
}
