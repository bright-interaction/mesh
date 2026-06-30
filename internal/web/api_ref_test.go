// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Bright Interaction AB

package web

import "testing"

func TestMCPToolsAndOpenAPI(t *testing.T) {
	s, _ := cfgServer(t)
	h := s.Handler()

	code, got := doJSON(t, h, "GET", "/api/mcp-tools", "")
	if code != 200 {
		t.Fatalf("mcp-tools = %d", code)
	}
	tools, _ := got["tools"].([]any)
	if len(tools) < 5 {
		t.Errorf("expected the MCP tool set, got %d", len(tools))
	}
	if got["contract"] == "" || got["config"] == nil {
		t.Errorf("mcp-tools missing contract/config: %+v", got)
	}

	code, spec := doJSON(t, h, "GET", "/openapi.json", "")
	if code != 200 || spec["openapi"] != "3.0.3" {
		t.Fatalf("openapi = %d, openapi=%v", code, spec["openapi"])
	}
	paths, _ := spec["paths"].(map[string]any)
	for _, want := range []string{"/api/search", "/api/config", "/api/note/{id}"} {
		if paths[want] == nil {
			t.Errorf("openapi missing path %s", want)
		}
	}
}
