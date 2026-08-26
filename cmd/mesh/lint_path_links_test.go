// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLintResolvesVaultRelativePathQualifiedLinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "decisions"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeNote(t, dir, "decisions/deploy.md", "---\nid: deploy\ntype: note\nwhen: 2026-01-01\n---\n# Deploy\n")
	writeNote(t, dir, "source.md", "---\nid: source\ntype: note\nwhen: 2026-01-01\n---\n# Source\n[[decisions/deploy]]\n")

	out, err := runCLI(t, rootCmd(), "lint", dir, "--all")
	if err != nil {
		t.Fatalf("lint returned %v:\n%s", err, out)
	}
	if strings.Contains(out, "resolves to nothing") {
		t.Fatalf("lint used absolute ParsedNote paths and broke a valid vault-relative link:\n%s", out)
	}
}
