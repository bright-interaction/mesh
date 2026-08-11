// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package index

import (
	"os"
	"path/filepath"
	"testing"
)

// Three packages create <vault>/.mesh: this one, pkg/meshclient and internal/ingest.
// The latter two ask for 0700 and put a 0600 credentials file and a 0600 sync.json in
// it. This one asked for 0755, and it is almost always the first to run, because every
// mesh command opens the index. MkdirAll is a no-op on an existing directory, so this
// package silently decided the mode for the other two, and the observed result on a
// real vault was a 0755 .mesh holding a 245MB mesh.db at 0644: the full text of every
// note in the vault, readable by any uid on the box, with the credentials file's own
// 0600 doing nothing about it.
func TestOpenKeepsVaultMeshDirPrivate(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this test asserts on")
	}
	tests := []struct {
		name   string
		preset os.FileMode // 0 = let Open create .mesh
		why    string
	}{
		{"fresh vault", 0, "a new .mesh must not be world-traversable"},
		{"a .mesh an older mesh created at 0755", 0o755, "tightening the constant does nothing for existing vaults unless the mode is repaired"},
		{"a .mesh at 0750, group-readable only", 0o750, "group traversal is still another uid reading credentials"},
		{"already private", 0o700, "the repair must be idempotent"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			meshDir := filepath.Join(root, ".mesh")
			if tc.preset != 0 {
				if err := os.MkdirAll(meshDir, tc.preset); err != nil {
					t.Fatal(err)
				}
				// MkdirAll honours umask, so force the mode we are simulating.
				if err := os.Chmod(meshDir, tc.preset); err != nil {
					t.Fatal(err)
				}
			}

			store, err := Open(root)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer store.Close()

			fi, err := os.Stat(meshDir)
			if err != nil {
				t.Fatal(err)
			}
			// Asserted against a LITERAL, not against vaultMeshDirMode. Comparing the
			// directory's mode to the constant that produced it is a tautology: widening
			// the constant would widen the directory and the test would still pass.
			if got := fi.Mode().Perm(); got != 0o700 {
				t.Errorf(".mesh mode = %04o, want 0700: %s", got, tc.why)
			}
		})
	}
}

// The repair must not cost the thing the directory exists for. A "secured" vault that
// cannot be indexed is just a broken one, and the mode is applied before the database
// is opened, so getting it wrong fails here rather than at some later write.
func TestOpenStillWorksAfterModeRepair(t *testing.T) {
	root := t.TempDir()
	meshDir := filepath.Join(root, ".mesh")
	if err := os.MkdirAll(meshDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(meshDir, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := Open(root)
	if err != nil {
		t.Fatalf("Open after repair: %v", err)
	}
	defer store.Close()
	if _, err := os.Stat(filepath.Join(meshDir, "mesh.db")); err != nil {
		t.Errorf("mesh.db missing after a repaired open: %v", err)
	}
}

// All three creators of <vault>/.mesh must agree on its mode. When they disagree the
// winner is whichever ran first and the loser's intent is discarded with no error
// anywhere, which is precisely how this bug survived a fix to one of the three.
func TestVaultMeshDirModeAgreesWithOtherWriters(t *testing.T) {
	// pkg/meshclient (writeCredentials, writeState) and internal/ingest (saveState)
	// both create the same directory at 0700. Hard-coded rather than imported: this
	// package must not depend on either, and the point is to catch a future edit that
	// moves one of them without the others.
	const otherWritersDirMode = 0o700
	if vaultMeshDirMode != otherWritersDirMode {
		t.Fatalf("vaultMeshDirMode = %04o but pkg/meshclient and internal/ingest create .mesh at %04o; three writers of one directory cannot disagree about its mode", vaultMeshDirMode, otherWritersDirMode)
	}
}
