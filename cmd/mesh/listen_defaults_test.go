// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"strings"
	"testing"

	"github.com/bright-interaction/mesh/internal/netaddr"
	"github.com/spf13/cobra"
)

// listenFlags are the flag names that make a command listen on a socket. Every
// command Mesh serves the vault from takes its bind address through one of these.
var listenFlags = []string{"addr", "http"}

// TestServingCommandsDefaultToLoopback walks the WHOLE command tree and fails if any
// listen flag defaults to an address that is not loopback.
//
// The tree, not a hand-written list of commands. `mesh serve-ssh` defaulted --addr to
// ":2222" while its own help said "localhost demos only" and `--allow-anonymous`
// served the entire vault to whoever connected: a list maintained by hand is exactly
// what let that sit next to `mesh ui` (127.0.0.1:7474) and `mesh mcp --http` (empty,
// stdio by default) without anyone noticing the odd one out. Walking the tree means a
// new serving command inherits the rule instead of having to be remembered.
//
// A default is what runs when the user thinks about nothing. Publishing a vault to
// every interface has to be something they typed on purpose.
func TestServingCommandsDefaultToLoopback(t *testing.T) {
	checked := 0
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, name := range listenFlags {
			f := c.Flags().Lookup(name)
			if f == nil {
				continue
			}
			checked++
			// Empty means "do not listen at all" (mesh mcp defaults to stdio).
			if strings.TrimSpace(f.DefValue) == "" {
				continue
			}
			if !netaddr.IsLoopback(f.DefValue) {
				t.Errorf("`%s --%s` defaults to %q, which binds every interface. "+
					"A serving command must default to loopback (e.g. 127.0.0.1:PORT); reaching the LAN "+
					"has to be something the user typed on purpose, alongside the auth that bind requires.",
					c.CommandPath(), name, f.DefValue)
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(rootCmd())
	// If the flags were renamed, this guard would silently watch nothing.
	if checked < 2 {
		t.Fatalf("found %d listen flags in the command tree, expected at least the ones on `mesh ui` and `mesh serve-ssh`; "+
			"if a flag was renamed, update listenFlags rather than leaving the guard pointing at nothing", checked)
	}
}
