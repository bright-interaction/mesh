// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bright-interaction/mesh/internal/hooks"
	"github.com/bright-interaction/mesh/internal/rerank"
	"github.com/bright-interaction/mesh/internal/shellpath"
	"github.com/spf13/cobra"
)

func rerankCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "rerank",
		Short: "Set up optional low-model subscription rerank for the local Mesh MCP server",
		Long:  "Manage subscription-backed rerank for a vault. Setup uses the Codex or Claude CLI's current subscription login, pins Mesh's small low-effort model, and stores the choice in a user-private config outside both the vault and project. It never makes a model call; authentication is checked by the first routed search.",
	}
	c.AddCommand(rerankSetupCmd(), rerankStatusCmd(), rerankDisableCmd())
	return c
}

func rerankSetupCmd() *cobra.Command {
	var client, dir, agent string
	c := &cobra.Command{
		Use:   "setup [vault]",
		Short: "Opt the local Mesh MCP server into Luna/low or Haiku/low reranking",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projAbs, err := filepath.Abs(dir)
			if err != nil {
				return err
			}
			if agent == "" {
				agent, err = inferRerankAgent(client)
				if err != nil {
					return err
				}
			}
			agent = strings.ToLower(strings.TrimSpace(agent))
			model, err := rerank.DefaultSubscriptionModel(agent)
			if err != nil {
				return err
			}
			bin, err := exec.LookPath(agent)
			if err != nil {
				return fmt.Errorf("%s CLI not found; install and sign in to it before enabling subscription rerank: %w", agent, err)
			}
			registered, p, err := hooks.MCPRegistered(client, projAbs)
			if err != nil {
				return err
			}
			if !registered {
				return fmt.Errorf("Mesh is not registered for %s in %s; run mesh install for that client first", client, p)
			}
			vaultAbs, err := filepath.Abs(vaultArg(args))
			if err != nil {
				return err
			}
			return installSubscriptionRerank(vaultAbs, agent, model, bin)
		},
	}
	c.Flags().StringVar(&client, "client", "claude-code", "agent client whose existing Mesh MCP registration to verify: "+strings.Join(hooks.Clients, ", "))
	c.Flags().StringVar(&dir, "dir", ".", "project dir (used for claude-code .mcp.json and vscode .vscode)")
	c.Flags().StringVar(&agent, "agent", "", "subscription CLI: codex or claude (defaults from --client when possible)")
	return c
}

func rerankStatusCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "status [vault]",
		Short: "Show the user-private per-vault rerank setting without spending quota",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			vaultAbs, err := filepath.Abs(vaultArg(args))
			if err != nil {
				return err
			}
			sub, enabled, p, err := rerank.LoadLocalSubscription(vaultAbs)
			if err != nil {
				return err
			}
			if !enabled {
				fmt.Fprintf(cmd.OutOrStdout(), "subscription rerank: off\nvault: %s\nconfig: %s\n", vaultAbs, p)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "subscription rerank: on\nprovider: %s\nmodel: %s\neffort: low\npolicy: %s\nvault: %s\nconfig: %s\n", sub.Agent, sub.Model, sub.Policy, vaultAbs, p)
			if bin, lookErr := exec.LookPath(sub.Agent); lookErr == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "CLI: found at %s\nauthentication: not checked (first routed search checks it)\n", bin)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "CLI: NOT FOUND (%v)\n", lookErr)
			}
			return nil
		},
	}
	return c
}

func rerankDisableCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "disable [vault]",
		Short: "Remove this vault's subscription setting from the user-private config",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			vaultAbs, err := filepath.Abs(vaultArg(args))
			if err != nil {
				return err
			}
			p, changed, err := rerank.RemoveLocalSubscription(vaultAbs)
			if err != nil {
				return err
			}
			if changed {
				fmt.Fprintf(cmd.OutOrStdout(), "subscription rerank disabled in %s\n", p)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "subscription rerank already off in %s\n", p)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Restart the agent client so it reloads the local Mesh MCP server.")
			return nil
		},
	}
	return c
}

func inferRerankAgent(client string) (string, error) {
	switch client {
	case "codex":
		return "codex", nil
	case "claude-code", "claude-desktop":
		return "claude", nil
	default:
		return "", fmt.Errorf("cannot infer a subscription CLI from client %q; pass --agent codex or --agent claude", client)
	}
}

func printSubscriptionRerankHint(client, vaultAbs string) {
	fmt.Println("\nOptional subscription rerank is off; Mesh stays zero-model by default.")
	if _, err := inferRerankAgent(client); err == nil {
		fmt.Printf("To opt into the pinned small model: mesh rerank setup %s --client %s\n", shellpath.Quote(vaultAbs), client)
		return
	}
	fmt.Printf("To opt in with Codex: mesh rerank setup %s --client %s --agent codex\n", shellpath.Quote(vaultAbs), client)
	fmt.Println("Use --agent claude instead for an existing Claude subscription login.")
}

func installSubscriptionRerank(vaultAbs, agent, model, bin string) error {
	policy := "auto"
	fmt.Println("  ! opt-in: routed searches send the query and up to 12 compact cards to the provider")
	p, changed, err := rerank.SaveLocalSubscription(vaultAbs, rerank.SubscriptionConfig{Agent: agent, Model: model, Policy: policy})
	if err != nil {
		return err
	}
	if changed {
		fmt.Printf("  + enabled subscription rerank for %s in %s\n", vaultAbs, p)
	} else {
		fmt.Printf("  . subscription rerank already configured in %s\n", p)
	}
	providerName := "Codex"
	if agent == "claude" {
		providerName = "Claude"
	}
	fmt.Printf("    %s CLI: %s\n", providerName, bin)
	fmt.Printf("    model: %s, effort: low, policy: auto\n", model)
	fmt.Println("    no model call was made; authentication is checked on the first routed search")
	fmt.Printf("    disable: mesh rerank disable %s\n", shellpath.Quote(vaultAbs))
	return nil
}
