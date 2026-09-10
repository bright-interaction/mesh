// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

// Package onboarding owns the small, client-neutral first-session marker shared
// by installer integrations and the open MCP server.
package onboarding

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Clients are the agent integrations supported by mesh install.
var Clients = []string{"claude-code", "claude-desktop", "cursor", "vscode", "windsurf", "codex"}

func marker(vaultRoot string) string { return filepath.Join(vaultRoot, ".mesh", "onboard-mcp") }

func sessionMarker(vaultRoot string) string { return filepath.Join(vaultRoot, ".mesh", "onboard") }

// SetSessionPending arms Claude Code's next SessionStart welcome.
func SetSessionPending(vaultRoot string) error {
	return setMarker(sessionMarker(vaultRoot), []byte("1"))
}

// ConsumeSessionPending atomically claims Claude Code's SessionStart welcome.
func ConsumeSessionPending(vaultRoot string) bool {
	data, ok := claimMarker(sessionMarker(vaultRoot))
	return ok && string(data) == "1"
}

// SetPending arms the next local MCP initialization for a one-time welcome. The
// client is an enum, not free-form prompt content.
func SetPending(vaultRoot, client string) error {
	if !validClient(client) {
		return fmt.Errorf("unknown client %q (use one of: %s)", client, strings.Join(Clients, ", "))
	}
	return setMarker(marker(vaultRoot), []byte(client+"\n"))
}

// ConsumePending returns the installing client to exactly one local MCP
// initializer. A malformed marker is removed and never enters instructions.
func ConsumePending(vaultRoot string) (string, bool) {
	data, ok := claimMarker(marker(vaultRoot))
	if !ok {
		return "", false
	}
	client := strings.TrimSpace(string(data))
	// Validate before returning so even a locally altered marker cannot inject
	// prompt text.
	if !validClient(client) {
		return "", false
	}
	return client, true
}

func setMarker(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600)
}

// claimMarker uses rename as the exclusive claim. Read-then-remove is not enough:
// concurrent readers can both observe the file before either removes it. Each
// contender creates a unique destination name, but only one can rename the shared
// source. The winner reads and clears its private claim file.
func claimMarker(p string) ([]byte, bool) {
	if _, err := os.Stat(p); err != nil {
		return nil, false
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, false
	}
	// A random, previously nonexistent destination makes Rename portable: Unix
	// may replace an existing destination while Windows refuses one, so collision
	// avoidance is part of the cross-platform atomicity contract.
	claim := fmt.Sprintf("%s-claim-%x", p, nonce)
	if err := os.Rename(p, claim); err != nil {
		return nil, false
	}
	defer os.Remove(claim)
	data, err := os.ReadFile(claim)
	if err != nil {
		return nil, false
	}
	return data, true
}

func validClient(client string) bool {
	for _, known := range Clients {
		if client == known {
			return true
		}
	}
	return false
}
