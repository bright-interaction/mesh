// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package ingest

import (
	"os"
	"path/filepath"
	"testing"
)

// .mesh/ingest-state.json sat at 0644 next to credentials and sync.json, both 0600. The
// audit that fixed sync.json left this one, on the reading that connector high-water
// marks are not sensitive. They name the source systems the vault pulls from, and they
// live in a directory this same file used to create at 0755, so it stays owner-only.
func TestSaveStateKeepsStatePrivate(t *testing.T) {
	tests := []struct {
		name    string
		presetD os.FileMode // 0 = let saveState create .mesh
		presetF os.FileMode // 0 = no pre-existing state file
	}{
		{"fresh vault", 0, 0},
		{"a .mesh another writer already made world-traversable", 0o755, 0},
		{"an existing state file left at 0644 by an older mesh", 0o755, 0o644},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			meshDir := filepath.Join(root, ".mesh")
			if tc.presetD != 0 {
				if err := os.MkdirAll(meshDir, tc.presetD); err != nil {
					t.Fatal(err)
				}
				// MkdirAll honours umask, so force the mode we are simulating.
				if err := os.Chmod(meshDir, tc.presetD); err != nil {
					t.Fatal(err)
				}
			}
			if tc.presetF != 0 {
				if err := os.WriteFile(filepath.Join(meshDir, "ingest-state.json"), []byte("{}"), tc.presetF); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(filepath.Join(meshDir, "ingest-state.json"), tc.presetF); err != nil {
					t.Fatal(err)
				}
			}

			if err := saveState(root, ingestState{LastRun: map[string]int64{"slack:acme": 1}}); err != nil {
				t.Fatal(err)
			}

			fi, err := os.Stat(statePath(root))
			if err != nil {
				t.Fatal(err)
			}
			// Asserted against a LITERAL, not against stateFileMode. Comparing a file's
			// mode to the constant that produced it is a tautology: widening the constant
			// would widen the file and the test would still pass, which is exactly what
			// ablating this test caught.
			if got := fi.Mode().Perm(); got != 0o600 {
				t.Errorf("ingest-state.json mode = %04o, want 0600: it names the source systems this vault pulls from and sits beside credentials", got)
			}

			// The round-trip must still work, or a "secure" state file is just a broken one.
			back, err := loadState(root)
			if err != nil {
				t.Fatal(err)
			}
			if back.LastRun["slack:acme"] != 1 {
				t.Errorf("state did not round-trip: %v", back.LastRun)
			}

			// No temp file may survive a successful write.
			entries, err := os.ReadDir(meshDir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if e.Name() != "ingest-state.json" {
					t.Errorf("leftover file in .mesh after a successful save: %s", e.Name())
				}
			}
		})
	}
}

// Both writers of .mesh must agree on its mode. MkdirAll is a no-op on an existing
// directory, so when they disagree the winner is whichever ran first and the loser's
// intent is discarded with no error anywhere.
func TestStateDirModeAgreesWithMeshclient(t *testing.T) {
	// pkg/meshclient creates the same directory at 0700; hard-coded here rather than
	// imported because internal/ must not depend on pkg/ for a constant.
	const meshclientDirMode = 0o700
	if stateDirMode != meshclientDirMode {
		t.Fatalf("stateDirMode = %04o but pkg/meshclient creates .mesh at %04o; two writers of one directory cannot disagree about its mode", stateDirMode, meshclientDirMode)
	}
}
