// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"encoding/json"
	"fmt"

	"github.com/bright-interaction/mesh/internal/buildinfo"
	"github.com/bright-interaction/mesh/internal/shellpath"
	"github.com/bright-interaction/mesh/pkg/meshclient"
	"github.com/spf13/cobra"
)

var runPrebuiltUpgrade = meshclient.UpgradePrebuilt

func upgradeCmd() *cobra.Command {
	var hubURL string
	var checkOnly, asJSON bool
	c := &cobra.Command{
		Use:   "upgrade [vault]",
		Short: "Upgrade a prebuilt Mesh client from its joined hub",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root := vaultArg(args)
			result, err := runPrebuiltUpgrade(cmd.Context(), meshclient.UpgradeOptions{
				VaultDir: root, HubURL: hubURL, CurrentRelease: buildinfo.ReleaseVer(), CheckOnly: checkOnly,
			})
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
			}
			switch {
			case result.UpToDate:
				fmt.Fprintf(cmd.OutOrStdout(), "Mesh %s is already current for %s.\n", result.Current, result.HubURL)
			case checkOnly:
				fmt.Fprintf(cmd.OutOrStdout(), "Mesh %s is available from %s (running %s).\nRun: mesh upgrade %s\n",
					result.Latest, result.HubURL, printableRelease(result.Current), shellpath.Quote(root))
			case result.Changed:
				fmt.Fprintf(cmd.OutOrStdout(), "Upgraded Mesh to %s at %s. Restart any running Mesh processes.\n", result.Latest, result.Path)
			}
			return nil
		},
	}
	c.Flags().StringVar(&hubURL, "hub", "", "hub base URL (defaults to this vault's joined hub)")
	c.Flags().BoolVar(&checkOnly, "check", false, "check the hub version without downloading or replacing anything")
	c.Flags().BoolVar(&asJSON, "json", false, "print a machine-readable result")
	return c
}

func printableRelease(v string) string {
	if v == "" {
		return "an unstamped build"
	}
	return v
}
