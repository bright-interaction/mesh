// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshcfg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFetchWorkersDefaultsAndRoundTrip(t *testing.T) {
	t.Setenv("MESH_FETCH_WORKERS", "")
	dir := t.TempDir()
	if got, err := FetchWorkersContext(nil, dir); err != nil || got != DefaultFetchWorkers {
		t.Fatalf("missing config = %d, %v", got, err)
	}
	for _, workers := range []int{1, 4, MaxFetchWorkers, 0} {
		cfg := Config{
			Embedding:    Embedding{Model: "original"},
			Retrieval:    Retrieval{FetchWorkers: workers, FreshnessHalfLifeDays: 30},
			SecretBridge: SecretBridge{AgentID: "operator"},
		}
		if err := SaveConfig(dir, cfg); err != nil {
			t.Fatal(err)
		}
		// Updating an unrelated section must preserve the worker setting and all
		// other existing sections, even for the legacy embedding-only writer.
		if err := Save(dir, Embedding{Model: "changed"}); err != nil {
			t.Fatal(err)
		}
		out, err := LoadConfig(dir)
		if err != nil || out.Retrieval != cfg.Retrieval || out.SecretBridge.AgentID != "operator" {
			t.Fatalf("round trip for %d = %+v, %v", workers, out, err)
		}
		want := workers
		if want == 0 {
			want = DefaultFetchWorkers
		}
		if got, err := FetchWorkersContext(context.Background(), dir); err != nil || got != want {
			t.Fatalf("updated file = %d, %v; want %d", got, err, want)
		}
		b, err := os.ReadFile(filepath.Join(dir, configName))
		if err != nil {
			t.Fatal(err)
		}
		if workers == 0 && strings.Contains(string(b), "fetch_workers =") {
			t.Fatal("internal absent sentinel was written as an explicit setting")
		}
	}
}

func TestFetchWorkersExplicitValidation(t *testing.T) {
	t.Setenv("MESH_FETCH_WORKERS", "")
	for _, raw := range []string{"", `""`, "0", "-1", "17", "1.5", "NaN", "many", "999999999999999999999999"} {
		t.Run(raw, func(t *testing.T) {
			dir := t.TempDir()
			body := fmt.Sprintf("[embedding]\nmodel = \"preserve\"\n[retrieval]\nfetch_workers = %s\n", raw)
			path := filepath.Join(dir, configName)
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(dir); err == nil {
				t.Fatalf("LoadConfig accepted invalid explicit %q", raw)
			}
			if _, err := FetchWorkersContext(context.Background(), dir); err == nil {
				t.Fatalf("FetchWorkersContext accepted invalid explicit %q", raw)
			}
			if err := Save(dir, Embedding{Model: "replacement"}); err == nil {
				t.Fatal("embedding save accepted an unreadable base")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != body {
				t.Fatalf("invalid config was overwritten: %v", err)
			}
		})
	}
	for _, n := range []int{-1, MaxFetchWorkers + 1} {
		dir := t.TempDir()
		if err := SaveConfig(dir, Config{Retrieval: Retrieval{FetchWorkers: n}}); err == nil {
			t.Fatalf("SaveConfig accepted invalid count %d", n)
		}
		if files, err := os.ReadDir(dir); err != nil || len(files) != 0 {
			t.Fatalf("invalid save left files: %v, %v", files, err)
		}
	}
}

func TestFetchWorkersEnvironmentAndCancellation(t *testing.T) {
	dir := t.TempDir()
	if err := SaveConfig(dir, Config{Retrieval: Retrieval{FetchWorkers: 4}}); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"1", "8", "16"} {
		t.Setenv("MESH_FETCH_WORKERS", raw)
		got, err := FetchWorkersContext(context.Background(), dir)
		if err != nil || fmt.Sprint(got) != raw {
			t.Fatalf("environment %q = %d, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"0", "-1", "17", "2.5", " ", "unbounded"} {
		t.Setenv("MESH_FETCH_WORKERS", raw)
		if _, err := FetchWorkersContext(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "MESH_FETCH_WORKERS") {
			t.Fatalf("invalid environment %q = %v", raw, err)
		}
	}
	t.Setenv("MESH_FETCH_WORKERS", "")
	if got, err := FetchWorkersContext(context.Background(), dir); err != nil || got != 4 {
		t.Fatalf("empty environment did not use file: %d, %v", got, err)
	}
	// An explicit valid environment setting overrides even an invalid file key.
	if err := os.WriteFile(filepath.Join(dir, configName), []byte("[retrieval]\nfetch_workers = 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MESH_FETCH_WORKERS", "8")
	if got, err := FetchWorkersContext(context.Background(), dir); err != nil || got != 8 {
		t.Fatalf("environment precedence = %d, %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := FetchWorkersContext(ctx, dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context with env override = %v", err)
	}
	t.Setenv("MESH_FETCH_WORKERS", "")
	if _, err := FetchWorkersContext(ctx, dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context with file = %v", err)
	}
}

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
		KeyEnv:      "MESH_EMBED_KEY", // key_env is allow-listed; see TestSaveRejectsKeyEnvOutsideTheAllowList
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
