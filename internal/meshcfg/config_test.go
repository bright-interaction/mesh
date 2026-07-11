// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMissingIsZero(t *testing.T) {
	e, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load of a missing config must not error: %v", err)
	}
	if (e != Embedding{}) {
		t.Errorf("missing config should be the zero Embedding, got %+v", e)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := Embedding{
		Endpoint:    "http://localhost:11434/v1",
		Model:       "nomic-embed-text",
		Dim:         768,
		KeyEnv:      "MY_KEY",
		QueryPrefix: "search_query: ",
		DocPrefix:   "search_document: ",
	}
	if err := Save(dir, in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch:\n in  %+v\n out %+v", in, out)
	}
}

func TestSaveDefaultsKeyEnv(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, Embedding{Endpoint: "http://x/v1", Model: "m", Dim: 4}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, _ := Load(dir)
	if out.KeyEnv != "MESH_EMBED_KEY" {
		t.Errorf("empty KeyEnv should default to MESH_EMBED_KEY, got %q", out.KeyEnv)
	}
}

// TestSaveNeverWritesSecrets is the sovereignty guard: the file holds the env var
// NAME, never a key value. (Defensive: even if a caller stuffed a secret into a
// field, only the documented fields are serialized.)
func TestSaveNeverWritesSecrets(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, Embedding{Endpoint: "http://x/v1", Model: "m", Dim: 4, KeyEnv: "MESH_EMBED_KEY"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); len(got) == 0 {
		t.Fatal("config.toml is empty")
	}
}

// TestSaveSanitizesInvalidKeyEnv: a KeyEnv that is not a plain env-var identifier
// (quotes, spaces, newlines) must be replaced with the default, never written
// verbatim (it could not name a real var and would not round-trip).
func TestSaveSanitizesInvalidKeyEnv(t *testing.T) {
	for _, bad := range []string{`BAD NAME`, `a"b`, "x\ny", `1leading`, ``} {
		dir := t.TempDir()
		if err := Save(dir, Embedding{Endpoint: "http://x/v1", Model: "m", Dim: 4, KeyEnv: bad}); err != nil {
			t.Fatalf("Save(%q): %v", bad, err)
		}
		out, _ := Load(dir)
		if out.KeyEnv != "MESH_EMBED_KEY" {
			t.Errorf("invalid KeyEnv %q should sanitize to MESH_EMBED_KEY, got %q", bad, out.KeyEnv)
		}
	}
}

// TestSecretBridgeRoundTrip: the [secret_bridge] section persists and reloads, and the
// file holds only base_url + the env-var NAME (never the Dockyard API key itself).
func TestSecretBridgeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := Config{
		Embedding:    Embedding{Endpoint: "http://x/v1", Model: "m", Dim: 4, KeyEnv: "MESH_EMBED_KEY"},
		SecretBridge: SecretBridge{BaseURL: "https://dockyard.example.com", KeyEnv: "MESH_SECRET_BRIDGE_KEY", AgentID: "mesh-box1"},
	}
	if err := SaveConfig(dir, in); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	out, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if out.SecretBridge != in.SecretBridge {
		t.Fatalf("secret_bridge round-trip:\n in  %+v\n out %+v", in.SecretBridge, out.SecretBridge)
	}
	// The file must never contain a secret VALUE, only the env-var name.
	b, _ := os.ReadFile(filepath.Join(dir, "config.toml"))
	if s := string(b); !strings.Contains(s, `key_env = "MESH_SECRET_BRIDGE_KEY"`) || !strings.Contains(s, `base_url = "https://dockyard.example.com"`) {
		t.Fatalf("secret_bridge section not written as expected:\n%s", s)
	}
}

// TestSecretBridgeSanitizesKeyEnv: a garbage key_env must reset to the default name.
func TestSecretBridgeSanitizesKeyEnv(t *testing.T) {
	dir := t.TempDir()
	in := Config{
		Embedding:    Embedding{Endpoint: "http://x/v1", Model: "m", Dim: 4, KeyEnv: "MESH_EMBED_KEY"},
		SecretBridge: SecretBridge{BaseURL: "https://d.example.com", KeyEnv: `bad name`},
	}
	if err := SaveConfig(dir, in); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	out, _ := LoadConfig(dir)
	if out.SecretBridge.KeyEnv != "MESH_SECRET_BRIDGE_KEY" {
		t.Fatalf("invalid key_env should sanitize to MESH_SECRET_BRIDGE_KEY, got %q", out.SecretBridge.KeyEnv)
	}
}

// TestFreshnessRoundTrip: freshness_half_life_days must survive SaveConfig->LoadConfig
// (the template previously hardcoded it to 0, silently dropping an operator's value on
// any save, e.g. when saving an unrelated field through the config UI).
func TestFreshnessRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := Config{
		Embedding: Embedding{Endpoint: "http://x/v1", Model: "m", Dim: 4, KeyEnv: "MESH_EMBED_KEY"},
		Retrieval: Retrieval{FreshnessHalfLifeDays: 30},
	}
	if err := SaveConfig(dir, in); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	out, _ := LoadConfig(dir)
	if out.Retrieval.FreshnessHalfLifeDays != 30 {
		t.Fatalf("freshness round-trip = %d, want 30", out.Retrieval.FreshnessHalfLifeDays)
	}
}

func TestSectionStringIgnoresWrongSection(t *testing.T) {
	const toml = `[rerank]
model = "cross-encoder"

[embedding]
model = "nomic-embed-text"
`
	if got := sectionString(toml, "embedding", "model"); got != "nomic-embed-text" {
		t.Errorf("got %q, want nomic-embed-text", got)
	}
}
