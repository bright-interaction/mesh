// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package meshcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSaveConfigLandsCompleteAndKeepsItsMode is the behavioural half of the durability
// fix. The fsyncs themselves cannot be observed from a unit test (a power-loss repro is
// not possible here), so they are pinned by the census guard in
// cmd/mesh/atomic_write_durability_test.go, which SaveConfig is no longer exempt from.
// What this test pins is everything the durability change could have broken while
// closing that gap:
//
//   - the file still lands COMPLETE, with every section round-tripping. A durable write
//     of half a config is not an improvement.
//   - the file still lands at 0644. os.CreateTemp makes its file 0600 and a rename
//     installs that mode over the destination, so a temp+rename writer that forgets the
//     chmod silently narrows the file it was only meant to make durable. That exact
//     regression was introduced elsewhere in this sweep, which is why it is pinned here
//     rather than trusted.
//   - no .config-*.toml temp file is left behind for a failed write to be mistaken for
//     a config, or for a vault walker to trip over.
func TestSaveConfigLandsCompleteAndKeepsItsMode(t *testing.T) {
	full := Config{
		Embedding: Embedding{
			Endpoint:    "http://localhost:11434/v1",
			Model:       "nomic-embed-text",
			Dim:         768,
			KeyEnv:      "MESH_EMBED_KEY",
			QueryPrefix: "search_query: ",
			DocPrefix:   "search_document: ",
		},
		Retrieval: Retrieval{
			WeightFTS:             1.5,
			WeightGraph:           0.75,
			WeightVec:             1.25,
			RerankEndpoint:        "http://localhost:8080/v1",
			RerankModel:           "bge-reranker-v2-m3",
			RerankKeyEnv:          "MESH_RERANK_KEY",
			RerankBlend:           0.4,
			HNSWThreshold:         20000,
			FreshnessHalfLifeDays: 90,
		},
		Code: Code{Index: true, Roots: []string{"/srv/repo-one", "/srv/repo-two"}, Languages: []string{"go", "ts"}},
		SecretBridge: SecretBridge{
			BaseURL: "https://vault.example.test",
			KeyEnv:  "MESH_SECRET_BRIDGE_KEY",
			AgentID: "mesh-laptop",
		},
	}

	cases := []struct {
		name     string
		existing os.FileMode // 0 means config.toml does not exist yet
		cfg      Config
		why      string
	}{
		{
			name:     "a fresh vault lands 0644, not the temp file's 0600",
			existing: 0,
			cfg:      full,
			why:      "config.toml has always been world readable; the durability fix must not narrow it",
		},
		{
			name:     "overwriting an existing config keeps 0644",
			existing: 0o644,
			cfg:      full,
			why:      "a rename installs the temp file's mode over the destination",
		},
		{
			name:     "an empty config still round-trips and lands 0644",
			existing: 0,
			cfg:      Config{},
			why:      "the zero config is what `mesh init` era vaults save; it must not be special-cased",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, configName)
			if tc.existing != 0 {
				if err := os.WriteFile(path, []byte("[embedding]\nendpoint = \"old\"\n"), tc.existing); err != nil {
					t.Fatal(err)
				}
				// Explicit chmod, because WriteFile's mode is filtered by umask.
				if err := os.Chmod(path, tc.existing); err != nil {
					t.Fatal(err)
				}
			}

			if err := SaveConfig(dir, tc.cfg); err != nil {
				t.Fatalf("SaveConfig: %v", err)
			}

			fi, err := os.Stat(path)
			if err != nil {
				t.Fatalf("config.toml missing after SaveConfig: %v", err)
			}
			if got := fi.Mode().Perm(); got != 0o644 {
				t.Errorf("mode = %04o, want 0644. %s", got, tc.why)
			}

			got, err := LoadConfig(dir)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			want := tc.cfg
			// SaveConfig fills the embedding key_env default; every other field must
			// come back exactly as written.
			if want.Embedding.KeyEnv == "" {
				want.Embedding.KeyEnv = "MESH_EMBED_KEY"
			}
			if got.Embedding != want.Embedding {
				t.Errorf("embedding = %+v, want %+v", got.Embedding, want.Embedding)
			}
			if got.Retrieval != want.Retrieval {
				t.Errorf("retrieval = %+v, want %+v", got.Retrieval, want.Retrieval)
			}
			if got.SecretBridge != want.SecretBridge {
				t.Errorf("secret_bridge = %+v, want %+v", got.SecretBridge, want.SecretBridge)
			}
			if got.Code.Index != want.Code.Index ||
				strings.Join(got.Code.Roots, ",") != strings.Join(want.Code.Roots, ",") ||
				strings.Join(got.Code.Languages, ",") != strings.Join(want.Code.Languages, ",") {
				t.Errorf("code = %+v, want %+v", got.Code, want.Code)
			}

			ents, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range ents {
				if strings.HasPrefix(e.Name(), ".config-") {
					t.Errorf("temp file %s left behind", e.Name())
				}
			}
		})
	}
}
