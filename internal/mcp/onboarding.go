// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/bright-interaction/mesh/internal/onboarding"
	"github.com/bright-interaction/mesh/internal/rerank"
	"github.com/bright-interaction/mesh/internal/shellpath"
)

// initializeInstructions keeps the permanent MCP contract tiny. Only the first
// trusted local stdio initialization after install receives the welcome; HTTP and
// hosted transports neither see nor consume the local marker.
func (s *Server) initializeInstructions(ctx context.Context) string {
	if !localOperator(ctx) {
		return contractText
	}
	client, ok := onboarding.ConsumePending(s.vaultRoot)
	if !ok {
		return contractText
	}

	var b strings.Builder
	b.WriteString("FIRST MESH SESSION: Briefly onboard the user before their task. Say Mesh is the shared knowledge vault you search before work and update after work, so this team does not relearn decisions. Say it is connected and indexed. Ask only: \"Want a 60-second tour, or should I continue with your task?\" If they choose the tour, call mesh_god_nodes, then one narrow mesh_search. Do not delay their task or change settings without permission.\n\n")
	b.WriteString("Mesh is zero-model by default: local FTS and graph retrieval spend no AI quota. ")

	if sub, enabled, _, err := rerank.LoadLocalSubscription(s.vaultRoot); err == nil && enabled {
		fmt.Fprintf(&b, "Optional subscription rerank is already enabled with %s/%s at low effort (%s policy); a routed call sends only the query and up to 12 compact cards, never full notes. ", sub.Agent, sub.Model, sub.Policy)
	} else if err == nil {
		b.WriteString(subscriptionOptInInstruction(client, s.vaultRoot))
	} else {
		b.WriteString("Do not claim subscription rerank is on or off because its local configuration could not be read; mention `mesh rerank status` only if the user asks. ")
	}

	b.WriteString("Do not run setup, test authentication, or spend quota unless the user explicitly opts in.\n\n---\n")
	b.WriteString(contractText)
	return b.String()
}

func subscriptionOptInInstruction(client, vaultRoot string) string {
	base := "If the user asks for stronger ranking, explain that it is optional and uses an existing subscription login, then offer `mesh rerank setup " + shellpath.Quote(vaultRoot) + " --client " + client
	switch client {
	case "codex":
		return base + "` (Luna, low effort). "
	case "claude-code", "claude-desktop":
		return base + "` (Haiku 4.5, low effort). "
	default:
		return base + " --agent codex` (Luna, low effort), or `--agent claude` for Haiku 4.5. "
	}
}
