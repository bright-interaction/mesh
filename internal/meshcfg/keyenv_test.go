// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshcfg

import "testing"

func TestKeyEnvAllowed(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"", true}, // empty = "use the built-in default"
		{"MESH_EMBED_KEY", true},
		{"MESH_RERANK_KEY", true},
		{"MESH_SECRET_BRIDGE_KEY", true},
		{"MESH_UI_TOKEN", false},                // break-glass admin login + cookie key seed
		{"MESH_UI_COOKIE_SECRET", false},        // member-cookie signing key
		{"MESH_HUB_TOKEN", false},               // hub client token
		{"MESH_HUB_SECRET_KEY", false},          // connector-secret at-rest key
		{"HOME", false},                         // any other process env var
		{"mesh_embed_key", false},               // the allow-list is exact, not case-folded
		{"MESH_EMBED_KEY ", false},              // no trimming: the caller must pass a clean name
		{"MESH_EMBED_KEY;MESH_UI_TOKEN", false}, // no separators
	}
	for _, tc := range cases {
		if got := KeyEnvAllowed(tc.name); got != tc.want {
			t.Errorf("KeyEnvAllowed(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The read-side gate: a config.toml hand-edited on the box (never through the web config
// API) must not make a read site dereference an unrelated process secret.
func TestResolveKeyEnvFallsBackForDisallowedNames(t *testing.T) {
	cases := []struct {
		configured, def, want string
	}{
		{"", "MESH_EMBED_KEY", "MESH_EMBED_KEY"},
		{"MESH_RERANK_KEY", "MESH_RERANK_KEY", "MESH_RERANK_KEY"},
		{"MESH_SECRET_BRIDGE_KEY", "MESH_EMBED_KEY", "MESH_SECRET_BRIDGE_KEY"},
		{"MESH_UI_TOKEN", "MESH_EMBED_KEY", "MESH_EMBED_KEY"},
		{"MESH_UI_COOKIE_SECRET", "MESH_RERANK_KEY", "MESH_RERANK_KEY"},
		{"AWS_SECRET_ACCESS_KEY", "MESH_SECRET_BRIDGE_KEY", "MESH_SECRET_BRIDGE_KEY"},
	}
	for _, tc := range cases {
		if got := ResolveKeyEnv("embedding.key_env", tc.configured, tc.def); got != tc.want {
			t.Errorf("ResolveKeyEnv(%q, def=%q) = %q, want %q", tc.configured, tc.def, got, tc.want)
		}
	}
}

// SaveConfig must not persist a disallowed key_env either, so a value that predates the
// allow-list is dropped the next time the file is rewritten instead of surviving forever.
func TestSaveRejectsKeyEnvOutsideTheAllowList(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Embedding:    Embedding{Endpoint: "http://x/v1", Model: "m", Dim: 4, KeyEnv: "MESH_UI_TOKEN"},
		Retrieval:    Retrieval{RerankKeyEnv: "MESH_UI_COOKIE_SECRET"},
		SecretBridge: SecretBridge{BaseURL: "https://dockyard.example", KeyEnv: "HOME"},
	}
	if err := SaveConfig(dir, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	out, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if out.Embedding.KeyEnv != "MESH_EMBED_KEY" {
		t.Errorf("embedding.key_env persisted as %q, want the default", out.Embedding.KeyEnv)
	}
	if out.Retrieval.RerankKeyEnv != "MESH_RERANK_KEY" {
		t.Errorf("rerank.key_env persisted as %q, want the default", out.Retrieval.RerankKeyEnv)
	}
	if out.SecretBridge.KeyEnv != "MESH_SECRET_BRIDGE_KEY" {
		t.Errorf("secret_bridge.key_env persisted as %q, want the default", out.SecretBridge.KeyEnv)
	}
}
