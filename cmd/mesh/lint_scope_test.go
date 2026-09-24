// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLintDoesNotCertifyOrTouchTheIndex(t *testing.T) {
	for _, brokenIndex := range []bool{false, true} {
		name := "absent_index"
		if brokenIndex {
			name = "broken_index"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeNote(t, root, "fixture.md", "---\nid: fixture\ntype: note\nwhen: 2026-01-01\n---\n# Fixture\n")
			indexPath := filepath.Join(root, ".mesh", "mesh.db")
			broken := []byte("not a SQLite database; lint must not repair this")
			if brokenIndex {
				if err := os.MkdirAll(filepath.Dir(indexPath), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(indexPath, broken, 0600); err != nil {
					t.Fatal(err)
				}
			}
			out, err := runCLI(t, rootCmd(), "lint", root, "--all")
			if err != nil {
				t.Fatalf("valid Markdown failed lint: %v: %s", err, out)
			}
			if strings.Contains(out, "retrieval is healthy") || !strings.Contains(out, "index freshness, database health and factual accuracy were not checked") {
				t.Fatalf("lint certified something it did not inspect: %s", out)
			}
			got, err := os.ReadFile(indexPath)
			if brokenIndex {
				if err != nil || !bytes.Equal(got, broken) {
					t.Fatalf("lint modified the index: err=%v match=%v", err, bytes.Equal(got, broken))
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("lint created or accessed an unexpected index: %v", err)
			}
		})
	}
}
