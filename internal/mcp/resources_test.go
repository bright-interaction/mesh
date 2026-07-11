// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package mcp

import (
	"context"
	"encoding/json"
	"testing"
)

// TestCapabilitiesResourceListsAllTools guards against drift: the mesh://capabilities
// resource must advertise EVERY tool in ToolSpecs(). It used to hardcode a slice that
// silently went stale on each new tool (it was missing mesh_health, the code tools,
// mesh_setup_hooks, and the secret tools).
func TestCapabilitiesResourceListsAllTools(t *testing.T) {
	s := newTestServer(t)
	out, rerr := s.handleResourcesRead(context.Background(), json.RawMessage(`{"uri":"mesh://capabilities"}`))
	if rerr != nil {
		t.Fatalf("capabilities read: %v", rerr)
	}
	m, _ := out.(map[string]any)
	contents, _ := m["contents"].([]map[string]any)
	if len(contents) == 0 {
		t.Fatalf("no contents in capabilities: %v", out)
	}
	var body struct {
		Tools []string `json:"tools"`
	}
	if err := json.Unmarshal([]byte(contents[0]["text"].(string)), &body); err != nil {
		t.Fatalf("capabilities body not json: %v", err)
	}
	if len(body.Tools) != len(ToolSpecs()) {
		t.Fatalf("capabilities lists %d tools, ToolSpecs has %d (drift)", len(body.Tools), len(ToolSpecs()))
	}
	has := func(name string) bool {
		for _, n := range body.Tools {
			if n == name {
				return true
			}
		}
		return false
	}
	if !has("mesh_secret_use") {
		t.Fatal("capabilities resource missing mesh_secret_use")
	}
}
